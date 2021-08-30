package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/ije/gox/utils"
	"github.com/postui/postdb"
	"github.com/postui/postdb/q"
)

type buildTask struct {
	id     string
	wd     string
	pkg    pkg
	alias  map[string]string
	deps   pkgSlice
	target string
	isDev  bool
	bundle bool
}

func (task *buildTask) aliasPrefix() string {
	alias := []string{}
	if len(task.alias) > 0 {
		var ss sort.StringSlice
		for name, to := range task.alias {
			ss = append(ss, fmt.Sprintf("%s:%s", name, to))
		}
		ss.Sort()
		alias = append(alias, fmt.Sprintf("alias:%s", strings.Join(ss, ",")))

	}
	if len(task.deps) > 0 {
		var ss sort.StringSlice
		for _, pkg := range task.deps {
			ss = append(ss, fmt.Sprintf("%s@%s", pkg.name, pkg.version))
		}
		ss.Sort()
		alias = append(alias, fmt.Sprintf("deps:%s", strings.Join(ss, ",")))
	}
	if len(alias) > 0 {
		return fmt.Sprintf("X-%s/", btoaUrl(strings.Join(alias, ",")))
	}
	return ""
}

func (task *buildTask) ID() string {
	if task.id != "" {
		return task.id
	}

	pkg := task.pkg
	name := path.Base(pkg.name)

	if pkg.submodule != "" {
		name = pkg.submodule
	}
	if strings.HasSuffix(name, ".js") {
		name = strings.TrimSuffix(name, ".js")
	}
	if task.isDev {
		name += ".development"
	}
	if task.bundle {
		name += ".bundle"
	}

	task.id = fmt.Sprintf(
		"v%d/%s@%s/%s%s/%s.js",
		VERSION,
		pkg.name,
		pkg.version,
		task.aliasPrefix(),
		task.target,
		name,
	)
	return task.id
}

func (task *buildTask) getImportPath(pkg pkg, extendsAlias bool) string {
	name := path.Base(pkg.name)
	if pkg.submodule != "" {
		name = pkg.submodule
	}
	if strings.HasSuffix(name, ".js") {
		name = strings.TrimSuffix(name, ".js")
	}
	if task.isDev {
		name += ".development"
	}

	var aliasPrefix string
	if extendsAlias {
		aliasPrefix = task.aliasPrefix()
	}

	return fmt.Sprintf(
		"/v%d/%s@%s/%s%s/%s.js",
		VERSION,
		pkg.name,
		pkg.version,
		aliasPrefix,
		task.target,
		name,
	)
}

func (task *buildTask) Build() (esm *ESM, pkgCSS bool, err error) {
	hasher := sha1.New()
	hasher.Write([]byte(task.ID()))
	task.wd = path.Join(os.TempDir(), "esm-build-"+hex.EncodeToString(hasher.Sum(nil)))
	ensureDir(task.wd)
	defer os.RemoveAll(task.wd)

	return task.build(newStringSet())
}

func (task *buildTask) build(tracing *stringSet) (esm *ESM, pkgCSS bool, err error) {
	if tracing.Has(task.ID()) {
		return findESM(task.ID())
	}
	tracing.Add(task.ID())

	nodeEnv := "production"
	if task.isDev {
		nodeEnv = "development"
	}

	esm, err = initESM(task.wd, task.pkg, task.deps, nodeEnv)
	if err != nil {
		log.Warn("init ESM:", err)
		return
	}
	defer func() {
		if err != nil {
			esm = nil
		}
	}()

	var entryPoint string
	var input *api.StdinOptions

	if esm.Module == "" {
		buf := bytes.NewBuffer(nil)
		importPath := task.pkg.ImportPath()
		if len(esm.Exports) > 0 {
			fmt.Fprintf(buf, `import * as __star from "%s";%s`, importPath, "\n")
			fmt.Fprintf(buf, `export const { %s } = __star;%s`, strings.Join(esm.Exports, ","), "\n")
		}
		fmt.Fprintf(buf, `export { default } from "%s";`, importPath)
		input = &api.StdinOptions{
			Contents:   buf.String(),
			ResolveDir: task.wd,
			Sourcefile: "mod.js",
		}
	} else {
		entryPoint = path.Join(task.wd, "node_modules", esm.Name, esm.Module)
	}

	minify := !task.isDev
	define := map[string]string{
		"__filename":                  fmt.Sprintf(`"https://%s/%s"`, config.cdnDomain, task.ID()),
		"__dirname":                   fmt.Sprintf(`"https://%s/%s"`, config.cdnDomain, path.Dir(task.ID())),
		"process":                     "__process$",
		"Buffer":                      "__Buffer$",
		"setImmediate":                "__setImmediate$",
		"clearImmediate":              "clearTimeout",
		"require.resolve":             "__rResolve$",
		"process.env.NODE_ENV":        fmt.Sprintf(`"%s"`, nodeEnv),
		"global":                      "__global$",
		"global.process":              "__process$",
		"global.Buffer":               "__Buffer$",
		"global.setImmediate":         "__setImmediate$",
		"global.clearImmediate":       "clearTimeout",
		"global.require.resolve":      "__rResolve$",
		"global.process.env.NODE_ENV": fmt.Sprintf(`"%s"`, nodeEnv),
	}
	external := newStringSet()
	extraExternal := newStringSet()
	esmResolverPlugin := api.Plugin{
		Name: "esm-resolver",
		Setup: func(plugin api.PluginBuild) {
			plugin.OnResolve(
				api.OnResolveOptions{Filter: ".*"},
				func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					specifier := strings.TrimSuffix(args.Path, "/")

					// resolve `?alias` parameter
					if len(task.alias) > 0 {
						if name, ok := task.alias[specifier]; ok {
							specifier = name
						}
					}

					// support nodejs builtin modules like `node:path`
					if strings.HasPrefix(specifier, "node:") {
						specifier = strings.TrimPrefix(specifier, "node:")
					}

					// bundles all dependencies except in `bundle` mode, apart from peer dependencies
					if task.bundle && !extraExternal.Has(specifier) {
						a := strings.Split(specifier, "/")
						pkgName := a[0]
						if len(a) > 1 && specifier[0] == '@' {
							pkgName = a[1]
						}
						if !builtInNodeModules[pkgName] {
							_, ok := esm.PeerDependencies[pkgName]
							if !ok {
								return api.OnResolveResult{}, nil
							}
						}
					}

					// splits modules based on the `exports` defines in package.json,
					// see https://nodejs.org/api/packages.html
					if strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") || specifier == ".." {
						resolvedPath := path.Join(path.Dir(args.Importer), specifier)
						// in macOS, the dir `/private/var/` is equal to `/var/`
						if strings.HasPrefix(resolvedPath, "/private/var/") {
							resolvedPath = strings.TrimPrefix(resolvedPath, "/private")
						}
						resolved := "." + strings.TrimPrefix(resolvedPath, path.Join(task.wd, "node_modules", esm.Name))
						m, ok := esm.DefinedExports.(map[string]interface{})
						if ok {
							for export, paths := range m {
								m, ok := paths.(map[string]interface{})
								if ok && export != "." {
									for _, value := range m {
										s, ok := value.(string)
										if ok && s != "" {
											match := resolved == s || resolved+".js" == s || resolved+".mjs" == s
											if !match {
												if a := strings.Split(s, "*"); len(a) == 2 {
													prefix := a[0]
													suffix := a[1]
													if (strings.HasPrefix(resolved, prefix)) &&
														(strings.HasSuffix(resolved, suffix) ||
															strings.HasSuffix(resolved+".js", suffix) ||
															strings.HasSuffix(resolved+".mjs", suffix)) {
														matchName := strings.TrimPrefix(strings.TrimSuffix(resolved, suffix), prefix)
														export = strings.Replace(export, "*", matchName, -1)
														match = true
													}
												}
											}
											if match {
												url := path.Join(esm.Name, export)
												if url == task.pkg.ImportPath() {
													return api.OnResolveResult{}, nil
												}
												external.Add(url)
												return api.OnResolveResult{Path: "ESM_SH_EXTERNAL:" + url, External: true}, nil
											}
										}
									}
								}
							}
						}
					}

					// bundles undefiend relative imports or the package/module it self
					if isLocalImport(specifier) || specifier == task.pkg.ImportPath() {
						return api.OnResolveResult{}, nil
					}

					// external
					external.Add(specifier)
					return api.OnResolveResult{Path: "ESM_SH_EXTERNAL:" + specifier, External: true}, nil
				},
			)
		},
	}

esbuild:
	start := time.Now()
	options := api.BuildOptions{
		Outdir:            "/esbuild",
		Write:             false,
		Bundle:            true,
		Target:            targets[task.target],
		Format:            api.FormatESModule,
		Platform:          api.PlatformBrowser,
		MinifyWhitespace:  minify,
		MinifyIdentifiers: minify,
		MinifySyntax:      minify,
		Plugins:           []api.Plugin{esmResolverPlugin},
	}
	if task.target == "node" {
		options.Platform = api.PlatformNode
	} else {
		options.Define = define
	}
	if entryPoint != "" {
		options.EntryPoints = []string{entryPoint}
	} else {
		options.Stdin = input
	}
	result := api.Build(options)
	if len(result.Errors) > 0 {
		// mark the missing module as external to exclude it from the bundle
		msg := result.Errors[0].Text
		if strings.HasPrefix(msg, "Could not resolve \"") && strings.Contains(msg, "mark it as external to exclude it from the bundle") {
			// but current package/module can not mark as external
			if strings.Contains(msg, fmt.Sprintf("Could not resolve \"%s\"", task.pkg.ImportPath())) {
				err = fmt.Errorf("Could not resolve \"%s\"", task.pkg.ImportPath())
				return
			}
			log.Warnf("esbuild(%s): %s", task.ID(), msg)
			name := strings.Split(msg, "\"")[1]
			if !extraExternal.Has(name) {
				extraExternal.Add(name)
				external.Add(name)
				goto esbuild
			}
		} else if strings.HasPrefix(msg, "No matching export in \"") && strings.Contains(msg, "for import \"default\"") {
			input = &api.StdinOptions{
				Contents:   fmt.Sprintf(`import "%s";export default null;`, task.pkg.ImportPath()),
				ResolveDir: task.wd,
				Sourcefile: "mod.js",
			}
			goto esbuild
		}
		err = errors.New("esbuild: " + msg)
		return
	}

	for _, w := range result.Warnings {
		log.Warnf("esbuild(%s): %s", task.ID(), w.Text)
	}

	cssMark := []byte{0}
	for _, file := range result.OutputFiles {
		outputContent := file.Contents
		if strings.HasSuffix(file.Path, ".js") {
			jsHeader := bytes.NewBufferString(fmt.Sprintf(
				"/* esm.sh - esbuild bundle(%s) %s %s */\n",
				task.pkg.String(),
				strings.ToLower(task.target),
				nodeEnv,
			))
			eol := "\n"
			if !task.isDev {
				eol = ""
			}

			// replace external imports/requires
			for _, name := range external.Values() {
				var importPath string
				// remote imports
				if isRemoteImport(name) {
					importPath = name
				}
				// is sub-module
				if importPath == "" && strings.HasPrefix(name, task.pkg.name+"/") {
					submodule := strings.TrimPrefix(name, task.pkg.name+"/")
					subPkg := pkg{
						name:      task.pkg.name,
						version:   task.pkg.version,
						submodule: submodule,
					}
					subTask := buildTask{
						wd:     task.wd,
						pkg:    subPkg,
						alias:  task.alias,
						deps:   task.deps,
						target: task.target,
						isDev:  task.isDev,
					}
					_, _, e := subTask.build(tracing)
					if e == nil {
						importPath = task.getImportPath(subPkg, true)
					}
				}
				// is builtin `buffer` module
				if importPath == "" && name == "buffer" {
					if task.target == "node" {
						importPath = "buffer"
					} else {
						importPath = fmt.Sprintf("/v%d/node_buffer.js", VERSION)
					}
				}
				// is builtin node module
				if importPath == "" && builtInNodeModules[name] {
					if task.target == "node" {
						importPath = name
					} else if task.target == "deno" && denoStdNodeModules[name] {
						importPath = fmt.Sprintf("/v%d/deno_std_node_%s.js", VERSION, name)
					} else {
						polyfill, ok := polyfilledBuiltInNodeModules[name]
						if ok {
							p, submodule, e := node.getPackageInfo(polyfill, "latest")
							if e != nil {
								err = e
								return
							}
							importPath = task.getImportPath(pkg{
								name:      p.Name,
								version:   p.Version,
								submodule: submodule,
							}, false)
						} else {
							f, err := embedFS.Open(fmt.Sprintf("embed/polyfills/node_%s.js", name))
							if err == nil {
								f.Close()
								importPath = fmt.Sprintf("/v%d/node_%s.js", VERSION, name)
							} else {
								importPath = fmt.Sprintf(
									"/error.js?type=unsupported-nodejs-builtin-module&name=%s&importer=%s",
									name,
									task.pkg.name,
								)
							}
						}
					}
				}
				// get package info via `deps` query
				if importPath == "" {
					for _, dep := range task.deps {
						if name == dep.name || strings.HasPrefix(name, dep.name+"/") {
							var submodule string
							if name != dep.name {
								submodule = strings.TrimPrefix(name, dep.name+"/")
							}
							importPath = task.getImportPath(pkg{
								name:      dep.name,
								version:   dep.version,
								submodule: submodule,
							}, false)
							break
						}
					}
				}
				// prer-build dependency
				if importPath == "" {
					var pkgName string
					var submodule string
					if a := strings.Split(name, "/"); strings.HasPrefix(name, "@") {
						if len(a) >= 2 {
							pkgName = strings.Join(a[:2], "/")
							submodule = strings.Join(a[2:], "/")
						}
					} else {
						pkgName = a[0]
						submodule = strings.Join(a[1:], "/")
					}

					packageFile := path.Join(task.wd, "node_modules", pkgName, "package.json")
					if fileExists(packageFile) {
						var p NpmPackage
						err = utils.ParseJSONFile(path.Join(task.wd, "node_modules", pkgName, "package.json"), &p)
						if err != nil {
							return
						}
						subTask := buildTask{
							wd: task.wd,
							pkg: pkg{
								name:      pkgName,
								version:   p.Version,
								submodule: submodule,
							},
							alias:  task.alias,
							deps:   task.deps,
							target: task.target,
							isDev:  task.isDev,
						}
						_, _, e := subTask.build(tracing)
						if e == nil {
							importPath = task.getImportPath(pkg{
								name:      p.Name,
								version:   p.Version,
								submodule: submodule,
							}, false)
						}
					}
				}
				// get package info from NPM
				if importPath == "" {
					version := "latest"
					if v, ok := esm.Dependencies[name]; ok {
						version = v
					} else if v, ok := esm.PeerDependencies[name]; ok {
						version = v
					}
					p, submodule, e := node.getPackageInfo(name, version)
					if e == nil {
						importPath = task.getImportPath(pkg{
							name:      p.Name,
							version:   p.Version,
							submodule: submodule,
						}, false)
					}
				}
				if importPath == "" {
					err = fmt.Errorf("Could not resolve \"%s\"  (Imported by \"%s\")", name, task.pkg.name)
					return
				}
				buf := bytes.NewBuffer(nil)
				identifier := identify(name)
				slice := bytes.Split(outputContent, []byte(fmt.Sprintf("\"ESM_SH_EXTERNAL:%s\"", name)))
				cjsContext := false
				cjsImported := false
				for i, p := range slice {
					if cjsContext {
						p = bytes.TrimPrefix(p, []byte{')'})
					}
					cjsContext = bytes.HasSuffix(p, []byte{'('})
					if cjsContext {
						shift := 0
						for i := len(p) - 2; i >= 0; i-- {
							c := p[i]
							if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$' {
								shift++
							} else {
								break
							}
						}
						if shift > 0 {
							p = p[0 : len(p)-(shift+1)]
						}
						if !cjsImported {
							wrote := false
							versionPrefx := fmt.Sprintf("/v%d/", VERSION)
							if strings.HasPrefix(importPath, versionPrefx) {
								pkg, err := parsePkg(strings.TrimPrefix(importPath, versionPrefx))
								if err == nil {
									// here the submodule should be always empty
									pkg.submodule = ""
									meta, err := initESM(task.wd, *pkg, task.deps, nodeEnv)
									// if the dependency is an es module without `default` export, then import star
									if err == nil && meta.Module != "" && !meta.ExportDefault {
										fmt.Fprintf(jsHeader, `import * as __%s$ from "%s";%s`, identifier, importPath, eol)
										wrote = true
									}
								}
							}
							if !wrote {
								fmt.Fprintf(jsHeader, `import __%s$ from "%s";%s`, identifier, importPath, eol)
							}
							cjsImported = true
						}
					}
					buf.Write(p)
					if i < len(slice)-1 {
						if cjsContext {
							buf.WriteString(fmt.Sprintf("__%s$", identifier))
						} else {
							buf.WriteString(fmt.Sprintf("\"%s\"", importPath))
						}
					}
				}
				outputContent = buf.Bytes()
			}

			// add nodejs/deno compatibility
			if task.target != "node" {
				if bytes.Contains(outputContent, []byte("__process$")) {
					fmt.Fprintf(jsHeader, `import __process$ from "/v%d/node_process.js";%s__process$.env.NODE_ENV="%s";%s`, VERSION, eol, nodeEnv, eol)
				}
				if bytes.Contains(outputContent, []byte("__Buffer$")) {
					fmt.Fprintf(jsHeader, `import { Buffer as __Buffer$ } from "/v%d/node_buffer.js";%s`, VERSION, eol)
				}
				if bytes.Contains(outputContent, []byte("__global$")) {
					fmt.Fprintf(jsHeader, `var __global$ = window;%s`, eol)
				}
				if bytes.Contains(outputContent, []byte("__setImmediate$")) {
					fmt.Fprintf(jsHeader, `var __setImmediate$ = (cb, ...args) => setTimeout(cb, 0, ...args);%s`, eol)
				}
				if bytes.Contains(outputContent, []byte("__rResolve$")) {
					fmt.Fprintf(jsHeader, `var __rResolve$ = p => p;%s`, eol)
				}
			}

			saveFilePath := path.Join(config.storageDir, "builds", task.ID())
			ensureDir(path.Dir(saveFilePath))

			var file *os.File
			file, err = os.Create(saveFilePath)
			if err != nil {
				return
			}
			defer file.Close()

			_, err = io.Copy(file, jsHeader)
			if err != nil {
				return
			}

			_, err = io.Copy(file, bytes.NewReader(outputContent))
			if err != nil {
				return
			}
		} else if strings.HasSuffix(file.Path, ".css") {
			saveFilePath := path.Join(config.storageDir, "builds", strings.TrimSuffix(task.ID(), ".js")+".css")
			ensureDir(path.Dir(saveFilePath))
			file, e := os.Create(saveFilePath)
			if e != nil {
				err = e
				return
			}
			defer file.Close()

			_, err = io.Copy(file, bytes.NewReader(outputContent))
			if err != nil {
				return
			}
			cssMark = []byte{1}
		}
	}

	log.Debugf("esbuild %s %s %s in %v", task.pkg.String(), task.target, nodeEnv, time.Now().Sub(start))

	err = task.handleDTS(esm)
	if err != nil {
		return
	}

	_, err = db.Put(
		q.Alias(task.ID()),
		q.KV{
			"esm": utils.MustEncodeJSON(esm),
			"css": cssMark,
		},
	)
	if err != nil && err == postdb.ErrDuplicateAlias {
		err = nil
	}
	if err != nil {
		return
	}

	pkgCSS = cssMark[0] == 1
	return
}

func (task *buildTask) handleDTS(esm *ESM) (err error) {
	start := time.Now()
	name, submodule := task.pkg.name, task.pkg.submodule
	versionedName := fmt.Sprintf("%s@%s", esm.Name, esm.Version)
	nodeModulesDir := path.Join(task.wd, "node_modules")

	var dts string
	if esm.Types != "" || esm.Typings != "" {
		dts = getTypesPath(task.wd, *esm.NpmPackage, "")
	} else if submodule == "" {
		if fileExists(path.Join(nodeModulesDir, name, "index.d.ts")) {
			dts = fmt.Sprintf("%s/%s", versionedName, "index.d.ts")
		} else if !strings.HasPrefix(name, "@") {
			packageFile := path.Join(nodeModulesDir, "@types", name, "package.json")
			if fileExists(packageFile) {
				var p NpmPackage
				err := utils.ParseJSONFile(path.Join(nodeModulesDir, "@types", name, "package.json"), &p)
				if err == nil {
					dts = getTypesPath(task.wd, p, "")
				}
			}
		}
	} else {
		if fileExists(path.Join(nodeModulesDir, name, submodule, "index.d.ts")) {
			dts = fmt.Sprintf("%s/%s", versionedName, path.Join(submodule, "index.d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, name, ensureSuffix(submodule, ".d.ts"))) {
			dts = fmt.Sprintf("%s/%s", versionedName, ensureSuffix(submodule, ".d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, "@types", name, submodule, "index.d.ts")) {
			dts = fmt.Sprintf("@types/%s/%s", versionedName, path.Join(submodule, "index.d.ts"))
		} else if fileExists(path.Join(nodeModulesDir, "@types", name, ensureSuffix(submodule, ".d.ts"))) {
			dts = fmt.Sprintf("@types/%s/%s", versionedName, ensureSuffix(submodule, ".d.ts"))
		}
	}
	if dts == "" {
		return
	}

	aliasPrefix := task.aliasPrefix()
	err = CopyDTS(
		task.wd,
		aliasPrefix,
		dts,
	)
	if err != nil {
		err = fmt.Errorf("copyDTS(%s:%s): %v", esm.Name, dts, err)
		return
	}
	esm.Dts = fmt.Sprintf("/%s%s", aliasPrefix, dts)
	log.Debugf("copy dts %s in %v", esm.Dts, time.Now().Sub(start))
	return
}
