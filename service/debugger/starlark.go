package debugger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	startime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/go-delve/delve/pkg/logflags"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/service/api"
)

//go:generate go run github.com/go-delve/build-tools/cmd/gen-starlark-bindings@latest go ./starlark_mapping.go
//go:generate go run github.com/go-delve/build-tools/cmd/gen-starlark-bindings@latest doc ../../../Documentation/cli/starlark.md

const (
	dlvCommandBuiltinName        = "dlv_command"
	appendFileBuiltinName        = "append_file"
	readFileBuiltinName          = "read_file"
	writeFileBuiltinName         = "write_file"
	commandPrefix                = "command_"
	dlvThreadLocalThreadName     = "dlv_thread"
	DlvThreadLocalRPCServer      = "dlv_rpc_server"
	curScopeBuiltinName          = "cur_scope"
	defaultLoadConfigBuiltinName = "default_load_config"
	targetObjectName             = "tgt"
	customPrettyPrintObjectName  = "PrettyPrint"
	goroutinesObjectName         = "gs"
	helpBuiltinName              = "help"
)

var defaultSyntaxFileOpts = &syntax.FileOptions{
	Set:             true,
	While:           true,
	TopLevelControl: true,
	GlobalReassign:  true,
	Recursion:       true,
}

func (d *Debugger) EvalStarlark(threadID uint64, s any, scope api.EvalScope, loadConfig api.LoadConfig, path, script string, flags api.EvalStarlarkFlags) (api.EvalStarlarkOut, error) {
	d.targetMutex.Lock()
	if flags&api.EvalStarlarkReset != 0 {
		d.StarlarkEnv.reset()
	}

	thread := d.StarlarkEnv.threads[threadID]
	if thread == nil {
		thread = d.StarlarkEnv.newThread(context.Background(), flags&api.EvalStarlarkNoninteractive == 0, s)
	}
	d.StarlarkEnv.scope = scope
	d.StarlarkEnv.loadConfig = loadConfig

	go d.StarlarkEnv.execute(thread, path, script, flags&api.EvalStarlarkCallCommand != 0)

	d.targetMutex.Unlock()
	return processStarlarkResp(thread)
}

func (d *Debugger) EvalStarlarkExpr(s any, scope api.EvalScope, loadConfig api.LoadConfig, script string, timeout time.Duration) (*api.Variable, error) {
	d.targetMutex.Lock()
	defer d.targetMutex.Unlock()
	to, _ := context.WithTimeout(context.Background(), timeout)
	thread := d.StarlarkEnv.newThread(to, false, nil)
	d.StarlarkEnv.scope = scope
	d.StarlarkEnv.loadConfig = loadConfig
	d.StarlarkEnv.isLocked = true
	defer func() {
		d.StarlarkEnv.isLocked = false
	}()
	out, err := d.StarlarkEnv.execStmt(thread, script)
	if err != nil {
		return nil, err
	}
	return starlarkValueToVariable(out), nil

}

func (d *Debugger) EvalStarlarkContinue(threadID uint64, value string, scope api.EvalScope, loadConfig api.LoadConfig, errIn string) (api.EvalStarlarkOut, error) {
	d.targetMutex.Lock()
	thread := d.StarlarkEnv.threads[threadID]
	if thread == nil {
		return api.EvalStarlarkOut{}, errors.New("unknown starlark thread")
	}
	d.StarlarkEnv.scope = scope
	d.StarlarkEnv.loadConfig = loadConfig
	thread.cont <- &starlarkCont{value, errIn}

	d.targetMutex.Unlock()
	return processStarlarkResp(thread)
}

func (d *Debugger) EvalStarlarkCancelAll() {
	for threadID := range d.StarlarkEnv.threads {
		d.StarlarkEnv.cancel(threadID)
	}
}

func (d *Debugger) EvalStarlarkCancel(threadID uint64) {
	d.StarlarkEnv.cancel(threadID)
}

func processStarlarkResp(thread *starlarkThread) (api.EvalStarlarkOut, error) {
	r := <-thread.resp
	if r.err != nil {
		return api.EvalStarlarkOut{}, r.err
	}
	r.ThreadID = thread.handle()
	r.IsCancelled = StarlarkThreadIsCancelled(thread.thread) != nil
	return r.EvalStarlarkOut, nil
}

// StarlarkEnv is the environment used to evaluate starlark scripts.
type StarlarkEnv struct {
	d *Debugger

	env, defaultEnv starlark.StringDict
	builtinDoc      map[string]string
	threads         map[uint64]*starlarkThread
	commands        map[string]starlarkCommand
	isLocked        bool

	scope      api.EvalScope
	loadConfig api.LoadConfig

	CustomPrettyPrinters map[string]*starlark.Function
}

type starlarkCommand struct {
	stringArgs bool
	fnval      *starlark.Function
}

type evalStarlarkOut struct {
	api.EvalStarlarkOut
	err error
}

type starlarkCont struct {
	value  string
	errstr string
}

func (cont *starlarkCont) error() error {
	if cont.errstr != "" {
		return errors.New(cont.errstr)
	}
	return nil
}

type starlarkThread struct {
	thread   *starlark.Thread
	ctx      context.Context
	resp     chan *evalStarlarkOut
	cont     chan *starlarkCont
	cancelfn context.CancelFunc
}

func (thread *starlarkThread) handle() uint64 {
	return uint64(uintptr(unsafe.Pointer(thread)))
}

// StarlarkEnvNew creates a new starlark binding environment.
func starlarkEnvNew(d *Debugger) *StarlarkEnv {
	env := &StarlarkEnv{
		d:                    d,
		env:                  make(starlark.StringDict),
		defaultEnv:           make(starlark.StringDict),
		commands:             make(map[string]starlarkCommand),
		threads:              make(map[uint64]*starlarkThread),
		builtinDoc:           make(map[string]string),
		CustomPrettyPrinters: make(map[string]*starlark.Function),
	}

	// Make the "time" module available to Starlark scripts.
	starlark.Universe["time"] = startime.Module

	env.defaultEnv[targetObjectName] = &starlarkTargetObject{env: env}
	env.defaultEnv[customPrettyPrintObjectName] = &starlarkCustomPrettyPrintObject{env: env}
	env.defaultEnv[goroutinesObjectName] = &starlarkGoroutinesObject{env: env}

	builtindoc := func(name, args, descr string) {
		env.builtinDoc[name] = name + args + "\n\n" + name + " " + descr
	}

	env.defaultEnv[dlvCommandBuiltinName] = starlark.NewBuiltin(dlvCommandBuiltinName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if err := StarlarkThreadIsCancelled(thread); err != nil {
			return starlark.None, err
		}
		argstrs := make([]string, len(args))
		for i := range args {
			a, ok := args[i].(starlark.String)
			if !ok {
				return nil, errors.New("argument of dlv_command is not a string")
			}
			argstrs[i] = string(a)
		}
		cont := clientRoundTrip(thread, "dlv_command", api.StarlarkDlvCommand, "", []string{strings.Join(argstrs, " ")})

		if cont.errstr != "" && strings.Contains(cont.errstr, " has exited with status ") {
			return env.InterfaceToStarlarkValue(cont.error()), nil
		}
		return starlark.None, DecorateStarlarkError(thread, cont.error())
	})
	builtindoc(dlvCommandBuiltinName, "(Command)", "interrupts, continues and steps through the program.")

	env.defaultEnv[readFileBuiltinName] = starlark.NewBuiltin(readFileBuiltinName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 1 {
			return nil, DecorateStarlarkError(thread, errors.New("wrong number of arguments"))
		}
		path, ok := args[0].(starlark.String)
		if !ok {
			return nil, DecorateStarlarkError(thread, errors.New("argument of read_file was not a string"))
		}
		cont := clientRoundTrip(thread, readFileBuiltinName, api.StarlarkReadFile, "", []string{string(path)})
		if cont.errstr != "" {
			return nil, DecorateStarlarkError(thread, cont.error())
		}
		return starlark.String(cont.value), nil
	})
	builtindoc(readFileBuiltinName, "(Path)", "reads a file.")

	env.env[appendFileBuiltinName] = starlark.NewBuiltin(appendFileBuiltinName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 2 {
			return nil, DecorateStarlarkError(thread, errors.New("wrong number of arguments"))
		}
		path, ok := args[0].(starlark.String)
		if !ok {
			return nil, DecorateStarlarkError(thread, errors.New("first argument of append_file was not a string"))
		}
		cont := clientRoundTrip(thread, writeFileBuiltinName, api.StarlarkAppendFile, "", []string{string(path), toString(args[1])})
		return starlark.None, DecorateStarlarkError(thread, cont.error())
	})
	builtindoc(appendFileBuiltinName, "(Path, Text)", "append text to the specified file.")

	env.defaultEnv[writeFileBuiltinName] = starlark.NewBuiltin(writeFileBuiltinName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 2 {
			return nil, DecorateStarlarkError(thread, errors.New("wrong number of arguments"))
		}
		path, ok := args[0].(starlark.String)
		if !ok {
			return nil, DecorateStarlarkError(thread, errors.New("first argument of write_file was not a string"))
		}
		cont := clientRoundTrip(thread, writeFileBuiltinName, api.StarlarkWriteFile, "", []string{string(path), toString(args[1])})
		return starlark.None, DecorateStarlarkError(thread, cont.error())
	})
	builtindoc(writeFileBuiltinName, "(Path, Text)", "writes text to the specified file.")

	env.defaultEnv[curScopeBuiltinName] = starlark.NewBuiltin(curScopeBuiltinName, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		return env.InterfaceToStarlarkValue(env.scope), nil
	})
	builtindoc(curScopeBuiltinName, "()", "returns the current scope.")

	env.defaultEnv[defaultLoadConfigBuiltinName] = starlark.NewBuiltin(defaultLoadConfigBuiltinName, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		return env.InterfaceToStarlarkValue(env.loadConfig), nil
	})
	builtindoc(defaultLoadConfigBuiltinName, "()", "returns the default load configuration.")

	env.defaultEnv[helpBuiltinName] = starlark.NewBuiltin(helpBuiltinName, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		out := new(bytes.Buffer)
		switch len(args) {
		case 0:
			fmt.Fprintln(out, "Available builtins:")
			bins := make([]string, 0, len(env.env))
			for name, value := range env.env {
				switch value.(type) {
				case *starlark.Builtin:
					bins = append(bins, name)
				}
			}
			sort.Strings(bins)
			for _, bin := range bins {
				fmt.Fprintf(out, "\t%s\n", bin)
			}
			fmt.Fprintf(out, "\n\nUse tgt.varname to access the varname variable in the target process (it is equivalent to 'eval(None, \"varname\").Variable.Value').\n")
			fmt.Fprintf(out, "\nUse the PrettyPrint variable to configure pretty printing.\n")
		case 1:
			switch x := args[0].(type) {
			case *starlark.Builtin:
				if env.builtinDoc[x.Name()] != "" {
					fmt.Fprintf(out, "%s\n", env.builtinDoc[x.Name()])
				} else {
					fmt.Fprintf(out, "no help for builtin %s\n", x.Name())
				}
			case *starlark.Function:
				fmt.Fprintf(out, "user defined function %s\n", x.Name())
				if doc := x.Doc(); doc != "" {
					fmt.Fprintln(out, doc)
				}
			default:
				fmt.Fprintf(out, "no help for object of type %T\n", args[0])
			}
		default:
			fmt.Fprintln(out, "wrong number of arguments ", len(args))
		}
		clientRoundTrip(thread, helpBuiltinName, api.StarlarkPrint, out.String(), nil)
		return starlark.None, nil
	})
	builtindoc(helpBuiltinName, "(Object)", "prints help for Object.")

	maps.Copy(env.env, env.defaultEnv)

	return env
}

func (env *StarlarkEnv) AddToDefaultEnv(builtins starlark.StringDict, doc map[string]string) {
	maps.Copy(env.defaultEnv, builtins)
	maps.Copy(env.env, builtins)
	maps.Copy(env.builtinDoc, doc)
}

func (env *StarlarkEnv) LoadConfig() api.LoadConfig {
	return env.loadConfig
}

func (env *StarlarkEnv) Scope() api.EvalScope {
	return env.scope
}

func (env *StarlarkEnv) reset() {
	for name := range env.env {
		switch {
		case strings.HasPrefix(name, commandPrefix):
			// keep commands
		case name[0] >= 'A' && name[0] <= 'Z':
			// keep globals
		default:
			delete(env.env, name)
		}
	}
	maps.Copy(env.env, env.defaultEnv)
}

func (env *StarlarkEnv) newThread(ctx context.Context, withCont bool, s any) *starlarkThread {
	thread := &starlark.Thread{
		Print: func(thread *starlark.Thread, msg string) {
			sthread := asStarlarkThread(thread)
			if sthread.cont == nil {
				logflags.DebuggerLogger().Error(msg)
			} else {
				c := clientRoundTrip(thread, "print", api.StarlarkPrint, msg, nil)
				if c.errstr != "" {
					logflags.DebuggerLogger().Errorf("starlark print error: %s", c.errstr)
				}
			}
		},
	}
	sthread := &starlarkThread{
		thread: thread,
		resp:   make(chan *evalStarlarkOut),
	}
	if withCont {
		sthread.cont = make(chan *starlarkCont)
	}
	sthread.ctx, sthread.cancelfn = context.WithCancel(ctx)
	thread.SetLocal(dlvThreadLocalThreadName, sthread)
	thread.SetLocal(DlvThreadLocalRPCServer, s)
	env.threads[sthread.handle()] = sthread
	return sthread
}

func (env *StarlarkEnv) execute(thread *starlarkThread, path, source string, callCommand bool) {
	defer func() {
		err := recover()
		if err == nil {
			return
		}
		logflags.Bug.Inc()
		errstr := new(bytes.Buffer)
		fmt.Fprintf(errstr, "panic executing starlark script: %v\n", err)
		for i := 0; ; i++ {
			pc, file, line, ok := runtime.Caller(i)
			if !ok {
				break
			}
			fname := "<unknown>"
			fn := runtime.FuncForPC(pc)
			if fn != nil {
				fname = fn.Name()
			}
			fmt.Fprintf(errstr, "%s\n\tin %s:%d\n", fname, file, line)
		}
		thread.resp <- &evalStarlarkOut{api.EvalStarlarkOut{Kind: api.StarlarkDone}, errors.New(errstr.String())}
	}()

	var out starlark.Value
	var err error
	if path != api.StarlarkStdin {
		var globals starlark.StringDict
		globals, err = starlark.ExecFileOptions(defaultSyntaxFileOpts, thread.thread, path, source, env.env)
		if err == nil {
			maps.Copy(env.env, globals)
		}
	} else if callCommand {
		cmdName, args, _ := strings.Cut(source, " ")
		cmd, cmdExists := env.commands[cmdName]
		if !cmdExists {
			err = fmt.Errorf("command %s does not exist", cmdName)
		} else {
			if cmd.stringArgs {
				_, err = starlark.Call(thread.thread, cmd.fnval, starlark.Tuple{starlark.String(args)}, nil)
			} else {
				var argval starlark.Value
				argval, err = starlark.EvalOptions(defaultSyntaxFileOpts, thread.thread, api.StarlarkStdin, "("+args+")", env.env)
				if err == nil {
					argtuple, ok := argval.(starlark.Tuple)
					if !ok {
						argtuple = starlark.Tuple{argval}
					}
					_, err = starlark.Call(thread.thread, cmd.fnval, argtuple, nil)
				}
			}
		}
	} else {
		out, err = env.execStmt(thread, source)
	}

	// Register new commands
	if thread.cont != nil {
		for name := range env.env {
			if strings.HasPrefix(name, commandPrefix) {
				if _, ok := env.commands[name]; ok {
					continue
				}
				env.createCommand(thread, name, env.env[name])
			}
		}
	}

	if err == nil && path != api.StarlarkStdin {
		_, err = env.callMain(thread.thread, env.env)
	}

	if err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			err = errors.New(evalErr.Backtrace())
		}
	}

	outstr := ""
	if out != nil {
		outstr = out.String()
	}
	thread.resp <- &evalStarlarkOut{
		api.EvalStarlarkOut{Kind: api.StarlarkDone, Output: outstr},
		err,
	}
}

func (env *StarlarkEnv) execStmt(thread *starlarkThread, source string) (starlark.Value, error) {
	f, err := syntax.Parse(api.StarlarkStdin, source, 0)
	if err != nil {
		return starlark.None, err
	}

	if expr := soleExpr(f); expr != nil {
		out, err := starlark.EvalExprOptions(defaultSyntaxFileOpts, thread.thread, expr, env.env)
		if out == starlark.None {
			out = nil
		}
		return out, err
	}
	// compile
	prog, err := starlark.FileProgram(f, env.env.Has)
	if err != nil {
		return starlark.None, err
	}

	res, err := prog.Init(thread.thread, env.env)
	if err != nil {
		return starlark.None, err
	}

	maps.Copy(env.env, res)

	return nil, nil
}

func (env *StarlarkEnv) callMain(thread *starlark.Thread, globals starlark.StringDict) (starlark.Value, error) {
	mainval := globals["main"]
	if mainval == nil {
		return starlark.None, nil
	}
	mainfn, ok := mainval.(*starlark.Function)
	if !ok {
		return starlark.None, errors.New("main is not a function")
	}
	if mainfn.NumParams() != 0 {
		return starlark.None, errors.New("wrong number of arguments for main")
	}
	return starlark.Call(thread, mainfn, make(starlark.Tuple, 0), nil)
}

// cancel cencels the execution of scripts on this thread
func (env *StarlarkEnv) cancel(threadID uint64) {
	thread := env.threads[threadID]
	if thread == nil {
		return
	}
	if thread.cancelfn != nil {
		thread.cancelfn()
		thread.cancelfn = nil
	}
	if thread.thread != nil {
		thread.thread.Cancel("user interrupt")
	}
	delete(env.threads, threadID)
}

func (env *StarlarkEnv) createCommand(thread *starlarkThread, name string, val starlark.Value) error {
	fnval, ok := val.(*starlark.Function)
	if !ok {
		return nil
	}

	name = name[len(commandPrefix):]

	helpMsg := fnval.Doc()
	if helpMsg == "" {
		helpMsg = "user defined"
	}

	stringArgs := false

	if fnval.NumParams() == 1 {
		if p0, _ := fnval.Param(0); p0 == "args" {
			stringArgs = true
		}
	}

	clientRoundTrip(thread.thread, "registering commands", api.StarlarkRegisterCommand, "", []string{name, helpMsg})
	env.commands[name] = starlarkCommand{stringArgs, fnval}
	return nil
}

func (env *StarlarkEnv) lock() (*proc.Target, func()) {
	if env.isLocked {
		return env.d.target.Selected, func() {}
	}
	tgt, unlock := env.d.LockTargetGroup()
	return tgt.Selected, unlock
}

func (env *StarlarkEnv) eval(scope api.EvalScope, expr string, timeout time.Duration, cfg proc.LoadConfig) (*proc.Variable, error) {
	s, err := proc.ConvertEvalScope(env.d.target.Selected, scope.GoroutineID, scope.Frame, scope.DeferredCall)
	if err != nil {
		return nil, err
	}
	return s.EvalExpression(expr, timeout, cfg)
}

func asStarlarkThread(thread *starlark.Thread) *starlarkThread {
	return thread.Local(dlvThreadLocalThreadName).(*starlarkThread)
}

func clientRoundTrip(thread *starlark.Thread, cmdName string, kind api.StarlarkOutKind, output string, args []string) *starlarkCont {
	sthread := asStarlarkThread(thread)
	if sthread.cont == nil {
		return &starlarkCont{errstr: fmt.Sprintf("%s not allowed in this environment", cmdName)}
	}

	sthread.resp <- &evalStarlarkOut{api.EvalStarlarkOut{Kind: kind, Args: args, Output: output}, nil}
	return <-sthread.cont
}

func StarlarkThreadIsCancelled(thread *starlark.Thread) error {
	sthread := asStarlarkThread(thread)
	select {
	case <-sthread.ctx.Done():
		return sthread.ctx.Err()
	default:
	}
	return nil
}

func DecorateStarlarkError(thread *starlark.Thread, err error) error {
	if err == nil {
		return nil
	}
	pos := thread.CallFrame(1).Pos
	if pos.Col > 0 {
		return fmt.Errorf("%s:%d:%d: %v", pos.Filename(), pos.Line, pos.Col, err)
	}
	return fmt.Errorf("%s:%d: %v", pos.Filename(), pos.Line, err)
}

func soleExpr(f *syntax.File) syntax.Expr {
	if len(f.Stmts) == 1 {
		if stmt, ok := f.Stmts[0].(*syntax.ExprStmt); ok {
			return stmt.X
		}
	}
	return nil
}

func toString(arg starlark.Value) string {
	switch v := arg.(type) {
	case starlark.String:
		return string(v)
	case starlark.Bytes:
		return string(v)
	default:
		return arg.String()
	}
}
