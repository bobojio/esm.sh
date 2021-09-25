package server

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// A Queue for esbuild
type BuildQueue struct {
	lock         sync.RWMutex
	list         *list.List
	tasks        map[string]*task
	processes    []*task
	maxProcesses int
}

type BuildQueueConsumer struct {
	C chan BuildOutput
}

type BuildOutput struct {
	esm *ESM
	err error
}

type task struct {
	*buildTask
	inProcess  bool
	el         *list.Element
	createTime time.Time
	startTime  time.Time
	consumers  []*BuildQueueConsumer
}

func (t *task) run() BuildOutput {
	c := make(chan BuildOutput, 1)
	go func(c chan BuildOutput) {
		esm, err := t.Build()
		c <- BuildOutput{esm, err}
	}(c)

	var output BuildOutput
	select {
	case output = <-c:
		if output.err != nil {
			log.Errorf("buildESM: %v", output.err)
		}
	case <-time.After(15 * time.Minute):
		output = BuildOutput{err: fmt.Errorf("build timeout")}
		log.Errorf("buildESM(%s): timeout", t.ID())
	}

	return output
}

func newBuildQueue(maxProcesses int) *BuildQueue {
	q := &BuildQueue{
		list:         list.New(),
		tasks:        map[string]*task{},
		maxProcesses: maxProcesses,
	}
	return q
}

// Len returns the number of tasks of the queue.
func (q *BuildQueue) Len() int {
	q.lock.RLock()
	defer q.lock.RUnlock()

	return q.list.Len()
}

// Add adds a new build task.
func (q *BuildQueue) Add(build *buildTask) *BuildQueueConsumer {
	c := &BuildQueueConsumer{make(chan BuildOutput, 1)}
	q.lock.Lock()
	t, ok := q.tasks[build.ID()]
	if ok {
		t.consumers = append(t.consumers, c)
	}
	q.lock.Unlock()

	if ok {
		return c
	}

	t = &task{
		buildTask:  build,
		createTime: time.Now(),
		consumers:  []*BuildQueueConsumer{c},
	}
	q.lock.Lock()
	t.el = q.list.PushBack(t)
	q.tasks[build.ID()] = t
	q.lock.Unlock()

	q.next()

	return c
}

func (q *BuildQueue) RemoveConsumer(task *buildTask, c *BuildQueueConsumer) {
	q.lock.Lock()
	defer q.lock.Unlock()

	t, ok := q.tasks[task.ID()]
	if ok {
		consumers := make([]*BuildQueueConsumer, len(t.consumers))
		i := 0
		for _, _c := range t.consumers {
			if _c != c {
				consumers[i] = c
				i++
			}
		}
		t.consumers = consumers[0:i]
	}
}

func (q *BuildQueue) next() {
	var nextTask *task
	q.lock.Lock()
	if len(q.processes) < q.maxProcesses {
		for el := q.list.Front(); el != nil; el = el.Next() {
			t, ok := el.Value.(*task)
			if ok && !t.inProcess {
				nextTask = t
				break
			}
		}
	}
	q.lock.Unlock()

	if nextTask == nil {
		return
	}

	q.lock.Lock()
	nextTask.inProcess = true
	q.processes = append(q.processes, nextTask)
	q.lock.Unlock()

	go q.wait(nextTask)
}

func (q *BuildQueue) wait(t *task) {
	t.startTime = time.Now()

	output := t.run()

	q.lock.Lock()
	a := make([]*task, len(q.processes))
	i := 0
	for _, _t := range q.processes {
		if _t != t {
			a[i] = _t
			i++
		}
	}
	q.processes = a[0:i]
	q.list.Remove(t.el)
	delete(q.tasks, t.ID())
	q.lock.Unlock()

	log.Debugf(
		"BuildQueue(%s,%s) done in %s",
		t.pkg.String(),
		t.target,
		time.Now().Sub(t.startTime),
	)
	q.next()

	for _, c := range t.consumers {
		c.C <- output
	}
}
