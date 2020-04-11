// package trace provides activity tracing via ctx through Tasks and Spans
//
// Basic Concepts
//
// Tracing can be used to identify where a piece of code spends its time.
//
// The Go standard library provides package runtime/trace which is useful to identify CPU bottlenecks or
// to understand what happens inside the Go runtime.
// However, it is not ideal for application level tracing, in particular if those traces should be understandable
// to tech-savvy users (albeit not developers).
//
// This package provides the concept of Tasks and Spans to express what activity is happening within an application:
//
//  - Neither task nor span is really tangible but instead contained within the context.Context tree
//  - Tasks represent concurrent activity (i.e. goroutines).
//  - Spans represent a semantic stack trace within a task.
//
// As a consequence, whenever a context is propagated across goroutine boundary, you need to create a child task:
//
//   go func(ctx context.Context) {
//     ctx, endTask = WithTask(ctx, "what-happens-inside-the-child-task")
//     defer endTask()
//     // ...
//   }(ctx)
//
// Within the task, you can open up a hierarchy of spans.
// In contrast to tasks, which have can multiple concurrently running child tasks,
// spans must nest and not cross the goroutine boundary.
//
//  ctx, endSpan = WithSpan(ctx, "copy-dir")
//  defer endSpan()
//  for _, f := range dir.Files() {
//    func() {
//      ctx, endSpan := WithSpan(ctx, fmt.Sprintf("copy-file %q", f))
//      defer endspan()
//      b, _ := ioutil.ReadFile(f)
//      _ = ioutil.WriteFile(f + ".copy", b, 0600)
//    }()
//  }
//
// In combination:
//  ctx, endTask = WithTask(ctx, "copy-dirs")
//  defer endTask()
//  for i := range dirs {
//    go func(dir string) {
//      ctx, endTask := WithTask(ctx, "copy-dir")
//      defer endTask()
//      for _, f := range filesIn(dir) {
//        func() {
//          ctx, endSpan := WithSpan(ctx, fmt.Sprintf("copy-file %q", f))
//          defer endspan()
//          b, _ := ioutil.ReadFile(f)
//          _ = ioutil.WriteFile(f + ".copy", b, 0600)
//        }()
//      }
//    }()
//  }
//
// Note that a span ends at the time you call endSpan - not before and not after that.
// If you violate the stack-like nesting of spans by forgetting an endSpan() invocation,
// the out-of-order endSpan() will panic.
//
// A similar rule applies to the endTask closure returned by WithTask:
// If a task has live child tasks at the time you call endTask(), the call will panic.
//
// Recovering from endSpan() or endTask() panics will corrupt the trace stack and lead to corrupt tracefile output.
//
//
// Best Practices For Naming Tasks And Spans
//
// Tasks should always have string constants as names, and must not contain the `#` character. WHy?
// First, the visualization by chrome://tracing draws a horizontal bar for each task in the trace.
// Also, the package appends `#NUM` for each concurrently running instance of a task name.
// Note that the `#NUM` suffix will be reused if a task has ended, in order to avoid an
// infinite number of horizontal bars in the visualization.
//
//
// Chrome-compatible Tracefile Support
//
// The activity trace generated by usage of WithTask and WithSpan can be rendered to a JSON output file
// that can be loaded into chrome://tracing .
// Apart from function GetSpanStackOrDefault, this is the main benefit of this package.
//
// First, there is a convenience environment variable 'ZREPL_ACTIVITY_TRACE' that can be set to an output path.
// From process start onward, a trace is written to that path.
//
// More consumers can attach to the activity trace through the ChrometraceClientWebsocketHandler websocket handler.
//
// If a write error is encountered with any consumer (including the env-var based one), the consumer is closed and
// will not receive further trace output.
package trace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/zrepl/zrepl/util/chainlock"
)

var metrics struct {
	activeTasks prometheus.Gauge
}
var taskNamer *uniqueConcurrentTaskNamer = newUniqueTaskNamer()

func init() {
	metrics.activeTasks = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "zrepl",
		Subsystem: "trace",
		Name:      "active_tasks",
		Help:      "number of active (tracing-level) tasks in the daemon",
	})
}

func RegisterMetrics(r prometheus.Registerer) {
	r.MustRegister(metrics.activeTasks)
}

type traceNode struct {
	id         string
	annotation string
	parentTask *traceNode

	mtx chainlock.L

	activeChildTasks int32 // only for task nodes, insignificant for span nodes
	parentSpan       *traceNode
	activeChildSpan  *traceNode // nil if task or span doesn't have an active child span

	startedAt time.Time
	endedAt   time.Time
}

func (s *traceNode) StartedAt() time.Time { return s.startedAt }
func (s *traceNode) EndedAt() time.Time   { return s.endedAt }

// Returned from WithTask or WithSpan.
// Must be called once the task or span ends.
// See package-level docs for nesting rules.
// Wrong call order / forgetting to call it will result in panics.
type DoneFunc func()

var ErrTaskStillHasActiveChildTasks = fmt.Errorf("end task: task still has active child tasks")

// Start a new root task or create a child task of an existing task.
//
// This is required when starting a new goroutine and
// passing an existing task context to it.
//
// taskName should be a constantand must not contain '#'
//
// The implementation ensures that,
// if multiple tasks with the same name exist simultaneously,
// a unique suffix is appended to uniquely identify the task opened with this function.
func WithTask(ctx context.Context, taskName string) (context.Context, DoneFunc) {

	var parentTask *traceNode
	nodeI := ctx.Value(contextKeyTraceNode)
	if nodeI != nil {
		node := nodeI.(*traceNode)
		if node.parentSpan != nil {
			parentTask = node.parentTask
		} else {
			parentTask = node
		}
	}
	// find the first ancestor that hasn't ended yet (nil if need be)
	if parentTask != nil {
		parentTask.mtx.Lock()
	}
	for parentTask != nil && !parentTask.endedAt.IsZero() {
		thisParent := parentTask
		parentTask = parentTask.parentTask
		// lock parent first such that it isn't modified by other callers to thisParent
		if parentTask != nil {
			parentTask.mtx.Lock()
		}
		thisParent.mtx.Unlock()
	}
	// invariant: either parentTask != nil and we hold the lock on parentTask, or parentTask is nil

	taskName, taskNameDone := taskNamer.UniqueConcurrentTaskName(taskName)

	this := &traceNode{
		id:               genID(),
		annotation:       taskName,
		parentTask:       parentTask,
		activeChildTasks: 0,
		parentSpan:       nil,
		activeChildSpan:  nil,

		startedAt: time.Now(),
		endedAt:   time.Time{},
	}

	if parentTask != nil {
		this.parentTask.activeChildTasks++
		parentTask.mtx.Unlock()
	}

	ctx = context.WithValue(ctx, contextKeyTraceNode, this)

	chrometraceBeginTask(this)

	metrics.activeTasks.Inc()

	endTaskFunc := func() {

		// only hold locks while manipulating the tree
		// (trace writer might block too long and unlike spans, tasks are updated concurrently)
		alreadyEnded := func() (alreadyEnded bool) {
			if this.parentTask != nil {
				defer this.parentTask.mtx.Lock().Unlock()
			}
			defer this.mtx.Lock().Unlock()

			if this.activeChildTasks != 0 {
				panic(errors.Wrapf(ErrTaskStillHasActiveChildTasks, "end task: %v active child tasks", this.activeChildTasks))
			}

			// support idempotent task ends
			if !this.endedAt.IsZero() {
				return true
			}
			this.endedAt = time.Now()

			if this.parentTask != nil {
				this.parentTask.activeChildTasks--
				if this.parentTask.activeChildTasks < 0 {
					panic("impl error: parent task with negative activeChildTasks count")
				}
			}
			return false
		}()
		if alreadyEnded {
			return
		}

		chrometraceEndTask(this)

		metrics.activeTasks.Dec()

		taskNameDone()
	}

	return ctx, endTaskFunc
}

var ErrAlreadyActiveChildSpan = fmt.Errorf("create child span: span already has an active child span")
var ErrSpanStillHasActiveChildSpan = fmt.Errorf("end span: span still has active child spans")

// Start a new span.
// Important: ctx must have an active task (see WithTask)
func WithSpan(ctx context.Context, annotation string) (context.Context, DoneFunc) {
	var parentSpan, parentTask *traceNode
	nodeI := ctx.Value(contextKeyTraceNode)
	if nodeI != nil {
		parentSpan = nodeI.(*traceNode)
		if parentSpan.parentSpan == nil {
			parentTask = parentSpan
		} else {
			parentTask = parentSpan.parentTask
		}
	} else {
		panic("must be called from within a task")
	}

	this := &traceNode{
		id:              genID(),
		annotation:      annotation,
		parentTask:      parentTask,
		parentSpan:      parentSpan,
		activeChildSpan: nil,

		startedAt: time.Now(),
		endedAt:   time.Time{},
	}

	parentSpan.mtx.HoldWhile(func() {
		if parentSpan.activeChildSpan != nil {
			panic(ErrAlreadyActiveChildSpan)
		}
		parentSpan.activeChildSpan = this
	})

	ctx = context.WithValue(ctx, contextKeyTraceNode, this)
	chrometraceBeginSpan(this)
	callbackEndSpan := callbackBeginSpan(ctx)

	endTaskFunc := func() {

		defer parentSpan.mtx.Lock().Unlock()
		if parentSpan.activeChildSpan != this && this.endedAt.IsZero() {
			panic("impl error: activeChildSpan should not change while != nil because there can only be one")
		}

		defer this.mtx.Lock().Unlock()
		if this.activeChildSpan != nil {
			panic(ErrSpanStillHasActiveChildSpan)
		}

		if !this.endedAt.IsZero() {
			return // support idempotent span ends
		}

		parentSpan.activeChildSpan = nil
		this.endedAt = time.Now()

		chrometraceEndSpan(this)
		callbackEndSpan(this)
	}

	return ctx, endTaskFunc
}

type StackKind struct {
	symbolizeTask func(t *traceNode) string
	symbolizeSpan func(s *traceNode) string
}

var (
	StackKindId = &StackKind{
		symbolizeTask: func(t *traceNode) string { return t.id },
		symbolizeSpan: func(s *traceNode) string { return s.id },
	}
	SpanStackKindCombined = &StackKind{
		symbolizeTask: func(t *traceNode) string { return fmt.Sprintf("(%s %q)", t.id, t.annotation) },
		symbolizeSpan: func(s *traceNode) string { return fmt.Sprintf("(%s %q)", s.id, s.annotation) },
	}
	SpanStackKindAnnotation = &StackKind{
		symbolizeTask: func(t *traceNode) string { return t.annotation },
		symbolizeSpan: func(s *traceNode) string { return s.annotation },
	}
)

func (n *traceNode) task() *traceNode {
	task := n.parentTask
	if n.parentSpan == nil {
		task = n
	}
	return task
}

func (n *traceNode) TaskName() string {
	task := n.task()
	return task.annotation
}

func (this *traceNode) TaskAndSpanStack(kind *StackKind) (spanIdStack string) {
	task := this.task()

	var spansInTask []*traceNode
	for s := this; s != nil; s = s.parentSpan {
		spansInTask = append(spansInTask, s)
	}

	var tasks []*traceNode
	for t := task; t != nil; t = t.parentTask {
		tasks = append(tasks, t)
	}

	var taskIdsRev []string
	for i := len(tasks) - 1; i >= 0; i-- {
		taskIdsRev = append(taskIdsRev, kind.symbolizeTask(tasks[i]))
	}

	var spanIdsRev []string
	for i := len(spansInTask) - 1; i >= 0; i-- {
		spanIdsRev = append(spanIdsRev, kind.symbolizeSpan(spansInTask[i]))
	}

	taskStack := strings.Join(taskIdsRev, "$")
	return fmt.Sprintf("%s$%s", taskStack, strings.Join(spanIdsRev, "."))
}

func GetSpanStackOrDefault(ctx context.Context, kind StackKind, def string) string {
	if nI := ctx.Value(contextKeyTraceNode); nI != nil {
		n := nI.(*traceNode)
		return n.TaskAndSpanStack(StackKindId)
	} else {
		return def
	}
}
