package debugger

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"reflect"

	"go.starlark.net/starlark"

	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
)

type starlarkTargetObject struct {
	starlarkUnhashable
	env *StarlarkEnv
}

var _ starlark.HasAttrs = &starlarkTargetObject{}

func (*starlarkTargetObject) String() string {
	return "<target variables>"
}

func (*starlarkTargetObject) Truth() starlark.Bool {
	return true
}

func (*starlarkTargetObject) Type() string {
	return "<target variables>"
}

func (tgt *starlarkTargetObject) AttrNames() []string {
	return nil
}

func (tgt *starlarkTargetObject) Attr(name string) (starlark.Value, error) {
	_, unlock := tgt.env.lock()
	v, err := tgt.env.eval(tgt.env.scope, name, 0, *api.LoadConfigToProc(&tgt.env.loadConfig))
	unlock()
	if err != nil {
		return starlark.None, fmt.Errorf("could not find variable %q: %v", name, err)
	}
	var customPrettyPrint api.CustomPrettyPrintFunc
	if !tgt.env.isLocked {
		customPrettyPrint = tgt.env.d.CustomPrettyPrint()
	}
	return tgt.env.variableValueToStarlarkValue(api.ConvertVar(v, customPrettyPrint), true)
}

var _ starlark.IterableMapping = &starlarkCustomPrettyPrintObject{}
var _ starlark.HasSetKey = &starlarkCustomPrettyPrintObject{}

type starlarkCustomPrettyPrintObject struct {
	starlarkUnhashable
	env *StarlarkEnv
}

func (*starlarkCustomPrettyPrintObject) String() string {
	return "<custom pretty printers>"
}

func (*starlarkCustomPrettyPrintObject) Truth() starlark.Bool {
	return true
}

func (*starlarkCustomPrettyPrintObject) Type() string {
	return "<custom pretty printers>"
}

func (pp *starlarkCustomPrettyPrintObject) Get(key starlark.Value) (starlark.Value, bool, error) {
	skey, ok := key.(starlark.String)
	if !ok {
		return starlark.None, false, errors.New("key is not a string")
	}
	v, ok := pp.env.CustomPrettyPrinters[string(skey)]
	return v, ok, nil
}

func (pp *starlarkCustomPrettyPrintObject) Items() []starlark.Tuple {
	r := make([]starlark.Tuple, 0, len(pp.env.CustomPrettyPrinters))
	for k, v := range pp.env.CustomPrettyPrinters {
		r = append(r, starlark.Tuple{starlark.String(k), v})
	}
	return r
}

func (pp *starlarkCustomPrettyPrintObject) Iterate() starlark.Iterator {
	next, stop := iter.Pull2(maps.All(pp.env.CustomPrettyPrinters))
	return starlarkCustomPrettyPrintObjectIterator{next, stop}
}

func (pp *starlarkCustomPrettyPrintObject) SetKey(key starlark.Value, value starlark.Value) error {
	skey, ok := key.(starlark.String)
	if !ok {
		return errors.New("key is not a string")
	}
	fnval, ok := value.(*starlark.Function)
	if !ok {
		return errors.New("value is not a function")
	}
	if fnval.NumParams() != 1 {
		return errors.New("wrong number of arguments for pretty print function")
	}
	pp.env.CustomPrettyPrinters[string(skey)] = fnval
	return nil
}

type starlarkCustomPrettyPrintObjectIterator struct {
	next func() (string, *starlark.Function, bool)
	stop func()
}

func (it starlarkCustomPrettyPrintObjectIterator) Next(p *starlark.Value) bool {
	k, v, ok := it.next()
	if !ok {
		return false
	}
	*p = starlark.Tuple{starlark.String(k), v}
	return true
}

func (it starlarkCustomPrettyPrintObjectIterator) Done() {
	it.stop()
}

type starlarkGoroutinesObject struct {
	starlarkUnhashable
	env *StarlarkEnv
}

var _ starlark.IterableMapping = &starlarkGoroutinesObject{}

func (sgo *starlarkGoroutinesObject) String() string {
	return "<target goroutines>"
}

func (sgo *starlarkGoroutinesObject) Truth() starlark.Bool {
	return true
}

func (sgo *starlarkGoroutinesObject) Type() string {
	return "<target goroutines>"
}

func (sgo *starlarkGoroutinesObject) Get(n starlark.Value) (starlark.Value, bool, error) {
	tgt, unlock := sgo.env.lock()
	defer unlock()
	switch n := n.(type) {
	case starlark.Int:
		if n, ok := n.Int64(); ok {
			if n == -1 {
				return &starlarkGoroutineObject{env: sgo.env, goid: -1}, true, nil
			}
			_, err := proc.FindGoroutine(tgt, n)
			if err != nil {
				return starlark.None, false, nil
			}
			return &starlarkGoroutineObject{env: sgo.env, goid: n}, true, nil
		} else {
			return starlark.None, false, errors.New("not an int64 integer")
		}
	default:
		return starlark.None, false, errors.New("not an integer")
	}
}

func (sgo *starlarkGoroutinesObject) Iterate() starlark.Iterator {
	return &starlarkGoroutinesIteratorObject{env: sgo.env}
}

func (sgo *starlarkGoroutinesObject) Items() []starlark.Tuple {
	return nil
}

type starlarkGoroutinesIteratorObject struct {
	env *StarlarkEnv

	gs        []*proc.G
	nextStart int
	curGoid   int64
}

func (it *starlarkGoroutinesIteratorObject) Done() {
}

func (it *starlarkGoroutinesIteratorObject) Next(p *starlark.Value) bool {
	tgt, unlock := it.env.lock()
	defer unlock()
	if len(it.gs) == 0 {
		if it.nextStart < 0 {
			return false
		}
		var err error
		it.gs, it.nextStart, err = proc.GoroutinesInfo(tgt, it.nextStart, 100)
		if err != nil {
			logflags.DebuggerLogger().Errorf("could not list goroutines: %v", err)
			return false
		}
		if len(it.gs) == 0 {
			return false
		}
	}

	it.curGoid = it.gs[0].ID
	it.gs = it.gs[1:]
	*p = &starlarkGoroutineObject{env: it.env, goid: it.curGoid}
	return true
}

type starlarkGoroutineObject struct {
	starlarkUnhashable
	env  *StarlarkEnv
	goid int64
}

var _ starlark.HasAttrs = &starlarkGoroutineObject{}

func (sgo *starlarkGoroutineObject) Truth() starlark.Bool {
	return true
}

func (sgo *starlarkGoroutineObject) String() string {
	starg, err := sgo.getStarlarkValue()
	if err != nil {
		return fmt.Sprintf("<error loading goroutine: %v>", err)
	}
	return starg.String()
}

func (sgo *starlarkGoroutineObject) Type() string {
	return "<goroutine>"
}

func (sgo *starlarkGoroutineObject) Attr(name string) (starlark.Value, error) {
	if name == "stack" {
		return &starlarkStackObject{env: sgo.env, goid: sgo.goid}, nil
	}
	starg, err := sgo.getStarlarkValue()
	if err != nil {
		return starlark.None, err
	}
	return starg.Attr(name)
}

func (sgo *starlarkGoroutineObject) getStarlarkValue() (structAsStarlarkValue, error) {
	tgt, unlock := sgo.env.lock()
	defer unlock()
	// One is tempted to cache this result, however if we did we would also
	// have to make sure that this caching does not survive a restart. Note
	// also that some caching is done in proc.
	g, err := proc.FindGoroutine(tgt, sgo.goid)
	if err != nil {
		return structAsStarlarkValue{}, err
	}
	return structAsStarlarkValue{env: sgo.env, v: reflect.ValueOf(api.ConvertGoroutine(tgt, g)).Elem()}, nil
}

func (sgo *starlarkGoroutineObject) AttrNames() []string {
	starg, err := sgo.getStarlarkValue()
	if err != nil {
		logflags.DebuggerLogger().Errorf("could not list attributes of goroutine %d: %v", sgo.goid, err)
		return nil
	}
	r := starg.AttrNames()
	return append(r, "stack")
}

type starlarkStackObject struct {
	starlarkUnhashable
	env  *StarlarkEnv
	goid int64
}

var _ starlark.IterableMapping = &starlarkStackObject{}

func (*starlarkStackObject) String() string {
	return "<target stack>"
}

func (*starlarkStackObject) Truth() starlark.Bool {
	return true
}

func (*starlarkStackObject) Type() string {
	return "<target stack>"
}

func (sso *starlarkStackObject) Get(key starlark.Value) (starlark.Value, bool, error) {
	tgt, unlock := sso.env.lock()
	defer unlock()
	var frameidx int64
	switch n := key.(type) {
	case starlark.Int:
		var ok bool
		frameidx, ok = n.Int64()
		if !ok {
			return starlark.None, false, errors.New("integer too big")
		}
	default:
		return starlark.None, false, errors.New("not an integer")
	}
	frames, err := starlarkGetFrames(tgt, sso.goid, int(frameidx))
	if err != nil {
		return starlark.None, false, err
	}
	if frames == nil {
		return starlark.None, false, nil
	}
	return &starlarkFrameObject{env: sso.env, goid: sso.goid, frame: int(frameidx)}, true, nil
}

func (sso *starlarkStackObject) Items() []starlark.Tuple {
	return nil
}

func (sso *starlarkStackObject) Iterate() starlark.Iterator {
	return &starlarkStackIterator{env: sso.env, goid: sso.goid}
}

type starlarkStackIterator struct {
	env  *StarlarkEnv
	goid int64

	idx    int
	frames []proc.Stackframe
}

func (*starlarkStackIterator) Done() {
}

func (it *starlarkStackIterator) Next(p *starlark.Value) bool {
	tgt, unlock := it.env.lock()
	defer unlock()
	if it.idx >= len(it.frames) {
		newsz := len(it.frames) * 2
		if newsz == 0 {
			newsz = 10
		}
		g, err := proc.FindGoroutine(tgt, it.goid)
		if err != nil {
			logflags.DebuggerLogger().Errorf("could not find goroutine %d: %v", it.goid, err)
			return false
		}
		if g == nil {
			it.frames, err = proc.ThreadStacktrace(tgt, tgt.CurrentThread(), newsz)
		} else {
			it.frames, err = proc.GoroutineStacktrace(tgt, g, newsz, 0)
		}
		if err != nil {
			logflags.DebuggerLogger().Errorf("could not get stacktrace of goroutine %d: %v", it.goid, err)
			return false
		}
	}
	if it.idx >= len(it.frames) {
		return false
	}
	*p = &starlarkFrameObject{env: it.env, goid: it.goid, frame: it.idx}
	it.idx++
	return true
}

type starlarkFrameObject struct {
	starlarkUnhashable
	env   *StarlarkEnv
	goid  int64
	frame int
}

var _ starlark.HasAttrs = &starlarkFrameObject{}

func (*starlarkFrameObject) Truth() starlark.Bool {
	return true
}

func (sfo *starlarkFrameObject) String() string {
	return sfo.getStarlarkValue().String()
}

func (*starlarkFrameObject) Type() string {
	return "<target stack frame>"
}

func (sfo *starlarkFrameObject) getStarlarkValue() structAsStarlarkValue {
	tgt, unlock := sfo.env.lock()
	defer unlock()
	frames, err := starlarkGetFrames(tgt, sfo.goid, sfo.frame)
	if err != nil {
		return structAsStarlarkValue{env: sfo.env, v: reflect.ValueOf(fmt.Sprintf("error getting frame: %v", err))}
	}
	frame := api.Stackframe{
		Location:           api.ConvertLocation(frames[0].Call),
		FrameOffset:        frames[0].FrameOffset(),
		FramePointerOffset: frames[0].FramePointerOffset(),
		Bottom:             frames[0].Bottom,
	}
	return structAsStarlarkValue{env: sfo.env, v: reflect.ValueOf(frame)}
}

func starlarkGetFrames(tgt *proc.Target, goid int64, frame int) ([]proc.Stackframe, error) {
	g, err := proc.FindGoroutine(tgt, goid)
	if err != nil {
		return nil, err
	}
	var frames []proc.Stackframe
	if g == nil {
		frames, err = proc.ThreadStacktrace(tgt, tgt.CurrentThread(), frame+1)
	} else {
		frames, err = proc.GoroutineStacktrace(tgt, g, frame+1, 0)
	}
	if err != nil {
		return nil, err
	}
	if frame >= len(frames) {
		return nil, nil
	}
	return frames[frame:], nil
}

func (sfo *starlarkFrameObject) Attr(name string) (starlark.Value, error) {
	if name == targetObjectName {
		return &starlarkFrameTargetObject{env: sfo.env, goid: sfo.goid, frame: sfo.frame}, nil
	}
	return sfo.getStarlarkValue().Attr(name)
}

func (sfo *starlarkFrameObject) AttrNames() []string {
	r := sfo.getStarlarkValue().AttrNames()
	return append(r, targetObjectName)
}

type starlarkFrameTargetObject struct {
	starlarkUnhashable
	env   *StarlarkEnv
	goid  int64
	frame int
}

var _ starlark.HasAttrs = &starlarkFrameTargetObject{}

func (*starlarkFrameTargetObject) Truth() starlark.Bool {
	return true
}

func (*starlarkFrameTargetObject) String() string {
	return "<target stack frame>"
}

func (*starlarkFrameTargetObject) Type() string {
	return "<target stack frame>"
}

func (sfto *starlarkFrameTargetObject) Attr(name string) (starlark.Value, error) {
	tgt, unlock := sfto.env.lock()
	g, _ := proc.FindGoroutine(tgt, sfto.goid)
	threadID := tgt.CurrentThread().ThreadID()
	if g != nil && g.Thread != nil {
		threadID = g.Thread.ThreadID()
	}
	frames, err := starlarkGetFrames(tgt, sfto.goid, sfto.frame)
	if err != nil {
		return starlark.None, err
	}
	scope := proc.FrameToScope(tgt, tgt.Memory(), g, threadID, frames...)
	v, err := scope.EvalExpression(name, 0, autoLoadConfig)
	unlock()
	if err != nil {
		return starlark.None, fmt.Errorf("could not find variable %q: %v", name, err)
	}
	var customPrettyPrint api.CustomPrettyPrintFunc
	if !sfto.env.isLocked {
		customPrettyPrint = sfto.env.d.CustomPrettyPrint()
	}
	return sfto.env.variableValueToStarlarkValue(api.ConvertVar(v, customPrettyPrint), true)

}

func (*starlarkFrameTargetObject) AttrNames() []string {
	return nil
}
