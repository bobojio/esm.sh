package server

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/ije/gox/utils"
)

func CopyDTS(wd string, resolvePrefix string, dts string) (err error) {
	return copyDTS(wd, resolvePrefix, dts, newStringSet())
}

func copyDTS(wd string, resolvePrefix string, dts string, tracing *stringSet) (err error) {
	// don't copy repeatly
	if tracing.Has(resolvePrefix + dts) {
		return
	}
	tracing.Add(resolvePrefix + dts)

	a := strings.Split(utils.CleanPath(dts)[1:], "/")
	versionedName := a[0]
	subPath := a[1:]
	if strings.HasPrefix(versionedName, "@") {
		versionedName = strings.Join(a[0:2], "/")
		subPath = a[2:]
	}
	pkgName, _ := utils.SplitByLastByte(versionedName, '@')
	if pkgName == "" {
		pkgName = versionedName
	}
	origin := ""
	if cdnDomain == "localhost" || strings.HasPrefix(cdnDomain, "localhost:") {
		origin = fmt.Sprintf("http://%s", cdnDomain)
	} else if cdnDomain != "" {
		origin = fmt.Sprintf("https://%s", cdnDomain)
	}

	dtsPath := utils.CleanPath(strings.Join(append([]string{
		fmt.Sprintf("/v%d", VERSION),
		versionedName,
		resolvePrefix,
	}, subPath...), "/"))
	savePath := "types" + dtsPath
	exists, _, err := fs.Exists(savePath)
	if err != nil || exists {
		return
	}

	imports := newStringSet()
	allDeclareModules := newStringSet()
	entryDeclareModules := []string{}

	dtsFilePath := path.Join(wd, "node_modules", regVersionPath.ReplaceAllString(dts, "$1/"))
	dtsDir := path.Dir(dtsFilePath)
	dtsFile, err := os.Open(dtsFilePath)
	if err != nil {
		return
	}

	dtsBuffer := bytes.NewBuffer(nil)
	err = walkDts(dtsFile, dtsBuffer, func(importPath string, kind string, position int) string {
		if kind == "declare module" {
			allDeclareModules.Add(importPath)
		}
		return importPath
	})
	// close the opened dts file
	dtsFile.Close()
	if err != nil {
		return
	}

	buf := bytes.NewBuffer(nil)
	if pkgName == "@types/node" {
		fmt.Fprintf(buf, "/// <reference path=\"%s/v%d/node.ns.d.ts\" />\n", origin, VERSION)
	}
	err = walkDts(dtsBuffer, buf, func(importPath string, kind string, position int) string {
		if kind == "declare module" {
			// resove `declare module "xxx" {}`, and the "xxx" must equal to the `moduleName`
			moduleName := pkgName
			if len(subPath) > 0 {
				moduleName += "/" + strings.Join(subPath, "/")
				if strings.HasSuffix(moduleName, "/index.d.ts") {
					moduleName = strings.TrimSuffix(moduleName, "/index.d.ts")
				} else if strings.HasSuffix(moduleName, ".d.ts") {
					moduleName = strings.TrimSuffix(moduleName, ".d.ts")
				}
			}
			if strings.HasPrefix(importPath, "node:") {
				importPath = "@types/node/" + strings.TrimPrefix(importPath, "node:")
			}
			if importPath == moduleName {
				if strings.HasPrefix(moduleName, "@types/node/") {
					return fmt.Sprintf("%s/v%d/%s.d.ts", origin, VERSION, moduleName)
				} else {
					res := fmt.Sprintf("%s/%s", origin, moduleName)
					entryDeclareModules = append(entryDeclareModules, fmt.Sprintf("%s:%d", moduleName, position+len(res)+1))
					return res
				}
			}
			return importPath
		}

		if allDeclareModules.Has(importPath) {
			return importPath
		}

		if isLocalImport(importPath) {
			if importPath == "." {
				importPath = "./index.d.ts"
			}
			if importPath == ".." {
				importPath = "../index.d.ts"
			}
			// some types is using `.js` extname
			if strings.HasSuffix(importPath, ".js") {
				importPath = strings.TrimSuffix(importPath, ".js")
			}
			if !strings.HasSuffix(importPath, ".d.ts") {
				if fileExists(path.Join(dtsDir, importPath, "index.d.ts")) {
					importPath = strings.TrimSuffix(importPath, "/") + "/index.d.ts"
				} else if fileExists(path.Join(dtsDir, importPath+".d.ts")) {
					importPath = importPath + ".d.ts"
				} else {
					var p NpmPackage
					packageJSONFile := path.Join(dtsDir, importPath, "package.json")
					if fileExists(packageJSONFile) && utils.ParseJSONFile(packageJSONFile, &p) == nil {
						if p.Types != "" {
							importPath = strings.TrimSuffix(importPath, "/") + utils.CleanPath(p.Types)
						} else if p.Typings != "" {
							importPath = strings.TrimSuffix(importPath, "/") + utils.CleanPath(p.Typings)
						}
					}
				}
			}
			if strings.HasSuffix(dts, ".d.ts") && !strings.HasSuffix(dts, "...d.ts") {
				imports.Add(importPath)
			}
		} else {
			if importPath == "node" {
				importPath = fmt.Sprintf("/v%d/node.ns.d.ts", VERSION)
			} else if strings.HasPrefix(importPath, "node:") {
				importPath = fmt.Sprintf("/v%d/@types/node/%s.d.ts", VERSION, strings.TrimPrefix(importPath, "node:"))
			} else if _, ok := builtInNodeModules[importPath]; ok {
				importPath = fmt.Sprintf("/v%d/@types/node/%s.d.ts", VERSION, importPath)
			} else {
				info, subpath, formPackageJSON, err := getPackageInfo(wd, importPath, "latest")
				if err != nil || ((info.Types == "" && info.Typings == "") && !strings.HasPrefix(info.Name, "@types/")) {
					info, _, formPackageJSON, err = getPackageInfo(wd, toTypesPackageName(importPath), "latest")
				}
				if err != nil {
					return importPath
				}

				if info.Types != "" || info.Typings != "" {
					versioned := info.Name + "@" + info.Version
					prefix := versioned + "/" + resolvePrefix
					// copy dependent dts files in the node_modules directory in current build context
					if formPackageJSON {
						importPath = toTypesPath(wd, info, subpath)
						if strings.HasSuffix(importPath, ".d.ts") && !strings.HasSuffix(importPath, "...d.ts") {
							imports.Add(importPath)
						}
						importPath = prefix + strings.TrimPrefix(importPath, versioned+"/")
					} else {
						if subpath == "" {
							if info.Types != "" {
								importPath = prefix + utils.CleanPath(info.Types)[1:]
							} else if info.Typings != "" {
								importPath = prefix + utils.CleanPath(info.Typings)[1:]
							}
						} else {
							importPath = prefix + utils.CleanPath(subpath)[1:]
						}
						if !strings.HasSuffix(importPath, ".d.ts") {
							importPath += "...d.ts"
						}
					}
					importPath = fmt.Sprintf("/v%d/%s", VERSION, importPath)
				}
			}

			// full CDN URL
			importPath = origin + importPath
		}

		return importPath
	})
	if err != nil {
		return
	}

	if len(entryDeclareModules) > 0 {
		dtsData := buf.Bytes()
		dataLen := buf.Len()
		for _, record := range entryDeclareModules {
			name, pos := utils.SplitByLastByte(record, ':')
			i, _ := strconv.Atoi(pos)
			b := bytes.NewBuffer(nil)
			open := false
			internal := 0
			for ; i < dataLen; i++ {
				c := dtsData[i]
				b.WriteByte(c)
				if c == '{' {
					if !open {
						open = true
					} else {
						internal++
					}
				} else if c == '}' && open {
					if internal > 0 {
						internal--
					} else {
						open = false
						break
					}
				}
			}
			if b.Len() > 0 {
				slice := strings.Split(name, "/")
				subpath := ""
				if l := len(slice); strings.HasPrefix(name, "@") && l > 1 {
					name = strings.Join(slice[:2], "/")
					if l > 2 {
						subpath = "/" + strings.Join(slice[2:], "/")
					}
				} else {
					name = slice[0]
					if l > 1 {
						subpath = "/" + strings.Join(slice[1:], "/")
					}
				}
				fmt.Fprintf(buf, `%sdeclare module "%s/%s@*%s" `, "\n", origin, name, subpath)
				fmt.Fprintf(buf, strings.TrimSpace(b.String()))
			}
		}
	}

	// workaroud for `@types/node`
	if pkgName == "@types/node" {
		dtsData := buf.Bytes()
		dtsData = bytes.ReplaceAll(dtsData, []byte(" implements NodeJS.ReadableStream"), []byte{})
		dtsData = bytes.ReplaceAll(dtsData, []byte(" implements NodeJS.WritableStream"), []byte{})
		buf = bytes.NewBuffer(dtsData)
	}

	err = fs.WriteData(savePath, buf.Bytes())
	if err != nil {
		return
	}

	for _, importDts := range imports.Values() {
		if isLocalImport(importDts) {
			if strings.HasPrefix(importDts, "/") {
				pkg, subpath := utils.SplitByFirstByte(importDts, '/')
				if strings.HasPrefix(pkg, "@") {
					n, _ := utils.SplitByFirstByte(subpath, '/')
					pkg = fmt.Sprintf("%s/%s", pkg, n)
				}
				importDts = path.Join(pkg, importDts)
			} else {
				importDts = path.Join(path.Dir(dts), importDts)
			}
		}
		err = copyDTS(wd, resolvePrefix, importDts, tracing)
		if err != nil {
			break
		}
	}

	return
}

func toTypesPath(wd string, p NpmPackage, subpath string) string {
	var types string
	if subpath != "" {
		types = subpath
		packageJSONFile := path.Join(wd, "node_modules", p.Name, subpath, "package.json")
		if fileExists(packageJSONFile) {
			var sp NpmPackage
			if utils.ParseJSONFile(packageJSONFile, &sp) == nil {
				if sp.Types != "" {
					types = path.Join(subpath, sp.Types)
				} else if sp.Typings != "" {
					types = path.Join(subpath, sp.Typings)
				}
			}
		}
	} else if p.Types != "" {
		types = p.Types
	} else if p.Typings != "" {
		types = p.Typings
	} else {
		return ""
	}

	if !strings.HasSuffix(types, ".d.ts") {
		pkgDir := path.Join(wd, "node_modules", p.Name)
		if fileExists(path.Join(pkgDir, types, "index.d.ts")) {
			types = types + "/index.d.ts"
		} else if fileExists(path.Join(pkgDir, types+".d.ts")) {
			types = types + ".d.ts"
		} else {
			types = types + "...d.ts" // dynamic
		}
	}

	return fmt.Sprintf("%s@%s%s", p.Name, p.Version, utils.CleanPath(types))
}
