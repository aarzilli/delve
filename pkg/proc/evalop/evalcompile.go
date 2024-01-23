package evalop

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"strconv"
	"strings"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/dwarf/reader"
)

var (
	ErrFuncCallNotAllowed         = errors.New("function calls not allowed without using 'call'")
	errFuncCallNotAllowedLitAlloc = errors.New("literal can not be allocated because function calls are not allowed without using 'call'")
	DebugPinnerFunctionName       = "runtime.debugPinner"
)

type compileCtx struct {
	evalLookup
	ops        []Op
	allowCalls bool
	curCall    int
	flags      Flags
	firstCall  bool
}

type evalLookup interface {
	FindTypeExpr(ast.Expr) (godwarf.Type, error)
	HasLocal(string) bool
	HasGlobal(string, string) bool
	HasBuiltin(string) bool
	LookupRegisterName(string) (int, bool)
}

type Flags uint8

const (
	CanSet Flags = 1 << iota
	HasDebugPinner
)

// CompileAST compiles the expression t into a list of instructions.
func CompileAST(lookup evalLookup, t ast.Expr, flags Flags) ([]Op, error) {
	ctx := &compileCtx{evalLookup: lookup, allowCalls: true, flags: flags, firstCall: true}
	err := ctx.compileAST(t)
	if err != nil {
		return nil, err
	}

	ctx.compileDebugUnpin()

	err = ctx.depthCheck(1)
	if err != nil {
		return ctx.ops, err
	}
	return ctx.ops, nil
}

// Compile compiles the expression expr into a list of instructions.
// If canSet is true expressions like "x = y" are also accepted.
func Compile(lookup evalLookup, expr string, flags Flags) ([]Op, error) {
	t, err := parser.ParseExpr(expr)
	if err != nil {
		if flags&CanSet != 0 {
			eqOff, isAs := isAssignment(err)
			if isAs {
				return CompileSet(lookup, expr[:eqOff], expr[eqOff+1:], flags)
			}
		}
		return nil, err
	}
	return CompileAST(lookup, t, flags)
}

func isAssignment(err error) (int, bool) {
	el, isScannerErr := err.(scanner.ErrorList)
	if isScannerErr && el[0].Msg == "expected '==', found '='" {
		return el[0].Pos.Offset, true
	}
	return 0, false
}

// CompileSet compiles the expression setting lhexpr to rhexpr into a list of
// instructions.
func CompileSet(lookup evalLookup, lhexpr, rhexpr string, flags Flags) ([]Op, error) {
	lhe, err := parser.ParseExpr(lhexpr)
	if err != nil {
		return nil, err
	}
	rhe, err := parser.ParseExpr(rhexpr)
	if err != nil {
		return nil, err
	}

	ctx := &compileCtx{evalLookup: lookup, allowCalls: true, flags: flags, firstCall: true}
	err = ctx.compileAST(rhe)
	if err != nil {
		return nil, err
	}

	if isStringLiteral(rhe) {
		ctx.compileAllocLiteralString()
	}

	err = ctx.compileAST(lhe)
	if err != nil {
		return nil, err
	}

	ctx.pushOp(&SetValue{lhe: lhe, Rhe: rhe})

	err = ctx.depthCheck(0)
	if err != nil {
		return ctx.ops, err
	}
	return ctx.ops, nil
}

func (ctx *compileCtx) compileAllocLiteralString() {
	jmp := &Jump{When: JumpIfAllocStringChecksFail}
	ctx.pushOp(jmp)

	ctx.compileSpecialCall("runtime.mallocgc", []ast.Expr{
		&ast.BasicLit{Kind: token.INT, Value: "0"},
		&ast.Ident{Name: "nil"},
		&ast.Ident{Name: "false"},
	}, []Op{
		&PushLen{},
		&PushNil{},
		&PushConst{constant.MakeBool(false)},
	}, true)

	ctx.pushOp(&ConvertAllocToString{})
	jmp.Target = len(ctx.ops)
}

func (ctx *compileCtx) compileSpecialCall(fnname string, argAst []ast.Expr, args []Op, doPinning bool) {
	if doPinning {
		ctx.compileGetDebugPinner()
	}

	id := ctx.curCall
	ctx.curCall++
	ctx.pushOp(&CallInjectionStartSpecial{
		id:     id,
		FnName: fnname,
		ArgAst: argAst})
	ctx.pushOp(&CallInjectionSetTarget{id: id})

	for i := range args {
		if args[i] != nil {
			ctx.pushOp(args[i])
		}
		ctx.pushOp(&CallInjectionCopyArg{id: id, ArgNum: i})
	}

	doPinning = doPinning && (ctx.flags&HasDebugPinner != 0)

	ctx.pushOp(&CallInjectionComplete{id: id, DoPinning: doPinning})

	if doPinning {
		ctx.compilePinningLoop(id)
	}
}

func (ctx *compileCtx) compileGetDebugPinner() {
	if ctx.firstCall && ctx.flags&HasDebugPinner != 0 {
		ctx.compileSpecialCall(DebugPinnerFunctionName, []ast.Expr{}, []Op{}, false)
		ctx.pushOp(&SetDebugPinner{})
		ctx.firstCall = false
	}
}

func (ctx *compileCtx) compileDebugUnpin() {
	if !ctx.firstCall && ctx.flags&HasDebugPinner != 0 {
		ctx.compileSpecialCall("runtime.(*Pinner).Unpin", []ast.Expr{
			&ast.Ident{Name: "debugPinner"},
		}, []Op{
			&PushDebugPinner{},
		}, false)
		ctx.pushOp(&Pop{})
		ctx.pushOp(&PushNil{})
		ctx.pushOp(&SetDebugPinner{})
	}
}

func (ctx *compileCtx) pushOp(op Op) {
	ctx.ops = append(ctx.ops, op)
}

// depthCheck validates the list of instructions produced by Compile and
// CompileSet by performing a stack depth check.
// It calculates the depth of the stack at every instruction in ctx.ops and
// checks that they have enough arguments to execute. For instructions that
// can be reached through multiple paths (because of a jump) it checks that
// all paths reach the instruction with the same stack depth.
// Finally it checks that the stack depth after all instructions have
// executed is equal to endDepth.
func (ctx *compileCtx) depthCheck(endDepth int) error {
	depth := make([]int, len(ctx.ops)+1) // depth[i] is the depth of the stack before i-th instruction
	for i := range depth {
		depth[i] = -1
	}
	depth[0] = 0

	var err error
	checkAndSet := func(j, d int) { // sets depth[j] to d after checking that we can
		if depth[j] < 0 {
			depth[j] = d
		}
		if d != depth[j] {
			err = fmt.Errorf("internal debugger error: depth check error at instruction %d: expected depth %d have %d (jump target)\n%s", j, d, depth[j], Listing(depth, ctx.ops))
		}
	}

	debugPinnerSeen := false

	for i, op := range ctx.ops {
		npop, npush := op.depthCheck()
		if depth[i] < npop {
			return fmt.Errorf("internal debugger error: depth check error at instruction %d: expected at least %d have %d\n%s", i, npop, depth[i], Listing(depth, ctx.ops))
		}
		d := depth[i] - npop + npush
		checkAndSet(i+1, d)
		switch op := op.(type) {
		case *Jump:
			checkAndSet(op.Target, d)
		case *CallInjectionStartSpecial:
			debugPinnerSeen = true
		case *CallInjectionComplete:
			if op.DoPinning && !debugPinnerSeen {
				err = fmt.Errorf("internal debugger error: pinning call injection seen before call to %s at instrution %d", DebugPinnerFunctionName, i)
			}
		}
		if err != nil {
			return err
		}
	}

	if depth[len(ctx.ops)] != endDepth {
		return fmt.Errorf("internal debugger error: depth check failed: depth at the end is not %d (got %d)\n%s", depth[len(ctx.ops)], endDepth, Listing(depth, ctx.ops))
	}
	return nil
}

func (ctx *compileCtx) compileAST(t ast.Expr) error {
	switch node := t.(type) {
	case *ast.CallExpr:
		return ctx.compileTypeCastOrFuncCall(node)

	case *ast.Ident:
		return ctx.compileIdent(node)

	case *ast.ParenExpr:
		// otherwise just eval recursively
		return ctx.compileAST(node.X)

	case *ast.SelectorExpr: // <expression>.<identifier>
		switch x := node.X.(type) {
		case *ast.Ident:
			switch {
			case x.Name == "runtime" && node.Sel.Name == "curg":
				ctx.pushOp(&PushCurg{})

			case x.Name == "runtime" && node.Sel.Name == "frameoff":
				ctx.pushOp(&PushFrameoff{})

			case x.Name == "runtime" && node.Sel.Name == "threadid":
				ctx.pushOp(&PushThreadID{})

			case ctx.HasLocal(x.Name):
				ctx.pushOp(&PushLocal{Name: x.Name})
				ctx.pushOp(&Select{node.Sel.Name})

			case ctx.HasGlobal(x.Name, node.Sel.Name):
				ctx.pushOp(&PushPackageVar{x.Name, node.Sel.Name})

			default:
				return ctx.compileUnary(node.X, &Select{node.Sel.Name})
			}

		case *ast.CallExpr:
			ident, ok := x.Fun.(*ast.SelectorExpr)
			if ok {
				f, ok := ident.X.(*ast.Ident)
				if ok && f.Name == "runtime" && ident.Sel.Name == "frame" {
					switch arg := x.Args[0].(type) {
					case *ast.BasicLit:
						fr, err := strconv.ParseInt(arg.Value, 10, 8)
						if err != nil {
							return err
						}
						// Push local onto the stack to be evaluated in the new frame context.
						ctx.pushOp(&PushLocal{Name: node.Sel.Name, Frame: fr})
						return nil
					default:
						return fmt.Errorf("expected integer value for frame, got %v", arg)
					}
				}
			}
			return ctx.compileUnary(node.X, &Select{node.Sel.Name})

		case *ast.BasicLit: // try to accept "package/path".varname syntax for package variables
			s, err := strconv.Unquote(x.Value)
			if err != nil {
				return err
			}
			if ctx.HasGlobal(s, node.Sel.Name) {
				ctx.pushOp(&PushPackageVar{s, node.Sel.Name})
				return nil
			}
			return ctx.compileUnary(node.X, &Select{node.Sel.Name})

		default:
			return ctx.compileUnary(node.X, &Select{node.Sel.Name})

		}

	case *ast.TypeAssertExpr: // <expression>.(<type>)
		return ctx.compileTypeAssert(node)

	case *ast.IndexExpr:
		return ctx.compileBinary(node.X, node.Index, nil, &Index{node})

	case *ast.SliceExpr:
		if node.Slice3 {
			return fmt.Errorf("3-index slice expressions not supported")
		}
		return ctx.compileReslice(node)

	case *ast.StarExpr:
		// pointer dereferencing *<expression>
		return ctx.compileUnary(node.X, &PointerDeref{node})

	case *ast.UnaryExpr:
		// The unary operators we support are +, - and & (note that unary * is parsed as ast.StarExpr)
		switch node.Op {
		case token.AND:
			return ctx.compileUnary(node.X, &AddrOf{node})
		default:
			return ctx.compileUnary(node.X, &Unary{node})
		}

	case *ast.BinaryExpr:
		switch node.Op {
		case token.INC, token.DEC, token.ARROW:
			return fmt.Errorf("operator %s not supported", node.Op.String())
		}
		// short circuits logical operators
		var sop *Jump
		switch node.Op {
		case token.LAND:
			sop = &Jump{When: JumpIfFalse, Node: node.X}
		case token.LOR:
			sop = &Jump{When: JumpIfTrue, Node: node.X}
		}
		err := ctx.compileBinary(node.X, node.Y, sop, &Binary{node})
		if err != nil {
			return err
		}
		if sop != nil {
			sop.Target = len(ctx.ops)
			ctx.pushOp(&BoolToConst{})
		}

	case *ast.BasicLit:
		ctx.pushOp(&PushConst{constant.MakeFromLiteral(node.Value, node.Kind, 0)})

	case *ast.CompositeLit:
		notimplerr := fmt.Errorf("expression %T not implemented", t)
		if ctx.flags&HasDebugPinner == 0 {
			return notimplerr
		}
		dtyp, err := ctx.FindTypeExpr(node.Type)
		if err != nil {
			return err
		}
		typ := ResolveTypedef(dtyp)
		switch typ := typ.(type) {
		case *godwarf.StructType:
			if !ctx.allowCalls {
				return errFuncCallNotAllowedLitAlloc
			}

			ctx.compileSpecialCall("runtime.mallocgc", []ast.Expr{
				&ast.BasicLit{Kind: token.INT, Value: "1"},
				node.Type,
				&ast.Ident{Name: "true"},
			}, []Op{
				&PushConst{Value: constant.MakeInt64(1)},
				&PushRuntimeType{dtyp},
				&PushConst{Value: constant.MakeBool(true)},
			}, true)
			ctx.pushOp(&TypeCast{DwarfType: &godwarf.PtrType{Type: dtyp}})
			ctx.pushOp(&PointerDeref{&ast.StarExpr{X: &ast.Ident{Name: "runtime.mallocgc"}}})

			for i, elt := range node.Elts {
				var field string
				var rhe ast.Expr
				switch elt := elt.(type) {
				case *ast.KeyValueExpr:
					rhe = elt.Value
					ctx.compileAST(elt.Value)
					field = elt.Key.(*ast.Ident).Name
				default:
					rhe = elt
					ctx.compileAST(elt)
					field = typ.Field[i].Name
				}
				ctx.pushOp(&Dup{})
				ctx.pushOp(&Select{Name: field})
				ctx.pushOp(&SetValue{Rhe: rhe})
			}

		case *godwarf.SliceType:
			return notimplerr

		case *godwarf.MapType:
			return notimplerr

		case *godwarf.ArrayType:
			return notimplerr

		default:
			return notimplerr
		}

	default:
		return fmt.Errorf("expression %T not implemented", t)
	}
	return nil
}

func (ctx *compileCtx) compileTypeCastOrFuncCall(node *ast.CallExpr) error {
	if len(node.Args) != 1 {
		// Things that have more or less than one argument are always function calls.
		return ctx.compileFunctionCall(node)
	}

	ambiguous := func() error {
		// Ambiguous, could be a function call or a type cast, if node.Fun can be
		// evaluated then try to treat it as a function call, otherwise try the
		// type cast.
		ctx2 := &compileCtx{evalLookup: ctx.evalLookup}
		err0 := ctx2.compileAST(node.Fun)
		if err0 == nil {
			return ctx.compileFunctionCall(node)
		}
		return ctx.compileTypeCast(node, err0)
	}

	fnnode := node.Fun
	for {
		fnnode = removeParen(fnnode)
		n, _ := fnnode.(*ast.StarExpr)
		if n == nil {
			break
		}
		fnnode = n.X
	}

	switch n := fnnode.(type) {
	case *ast.BasicLit:
		// It can only be a ("type string")(x) type cast
		return ctx.compileTypeCast(node, nil)
	case *ast.ArrayType, *ast.StructType, *ast.FuncType, *ast.InterfaceType, *ast.MapType, *ast.ChanType:
		return ctx.compileTypeCast(node, nil)
	case *ast.SelectorExpr:
		if _, isident := n.X.(*ast.Ident); isident {
			if typ, _ := ctx.FindTypeExpr(n); typ != nil {
				return ctx.compileTypeCast(node, nil)
			}
			return ambiguous()
		}
		return ctx.compileFunctionCall(node)
	case *ast.Ident:
		if ctx.HasBuiltin(n.Name) {
			return ctx.compileFunctionCall(node)
		}
		if ctx.HasGlobal("", n.Name) || ctx.HasLocal(n.Name) {
			return ctx.compileFunctionCall(node)
		}
		return ctx.compileTypeCast(node, fmt.Errorf("could not find symbol value for %s", n.Name))
	case *ast.IndexExpr:
		// Ambiguous, could be a parametric type
		switch n.X.(type) {
		case *ast.Ident, *ast.SelectorExpr:
			// Do the type-cast first since evaluating node.Fun could be expensive.
			err := ctx.compileTypeCast(node, nil)
			if err == nil || err != reader.ErrTypeNotFound {
				return err
			}
			return ctx.compileFunctionCall(node)
		default:
			return ctx.compileFunctionCall(node)
		}
	case *astIndexListExpr:
		return ctx.compileTypeCast(node, nil)
	default:
		// All other expressions must be function calls
		return ctx.compileFunctionCall(node)
	}
}

func (ctx *compileCtx) compileTypeCast(node *ast.CallExpr, ambiguousErr error) error {
	err := ctx.compileAST(node.Args[0])
	if err != nil {
		return err
	}

	fnnode := node.Fun

	// remove all enclosing parenthesis from the type name
	fnnode = removeParen(fnnode)

	targetTypeStr := ExprToString(removeParen(node.Fun))
	styp, err := ctx.FindTypeExpr(fnnode)
	if err != nil {
		switch targetTypeStr {
		case "[]byte", "[]uint8":
			styp = godwarf.FakeSliceType(godwarf.FakeBasicType("uint", 8))
		case "[]int32", "[]rune":
			styp = godwarf.FakeSliceType(godwarf.FakeBasicType("int", 32))
		default:
			if ambiguousErr != nil && err == reader.ErrTypeNotFound {
				return fmt.Errorf("could not evaluate function or type %s: %v", ExprToString(node.Fun), ambiguousErr)
			}
			return err
		}
	}

	ctx.pushOp(&TypeCast{DwarfType: styp, Node: node})
	return nil
}

func (ctx *compileCtx) compileBuiltinCall(builtin string, args []ast.Expr) error {
	for _, arg := range args {
		err := ctx.compileAST(arg)
		if err != nil {
			return err
		}
	}
	ctx.pushOp(&BuiltinCall{builtin, args})
	return nil
}

func (ctx *compileCtx) compileIdent(node *ast.Ident) error {
	switch {
	case ctx.HasLocal(node.Name):
		ctx.pushOp(&PushLocal{Name: node.Name})
	case ctx.HasGlobal("", node.Name):
		ctx.pushOp(&PushPackageVar{"", node.Name})
	case node.Name == "true" || node.Name == "false":
		ctx.pushOp(&PushConst{constant.MakeBool(node.Name == "true")})
	case node.Name == "nil":
		ctx.pushOp(&PushNil{})
	default:
		found := false
		if regnum, ok := ctx.LookupRegisterName(node.Name); ok {
			ctx.pushOp(&PushRegister{regnum, node.Name})
			found = true
		}
		if !found {
			return fmt.Errorf("could not find symbol value for %s", node.Name)
		}
	}
	return nil
}

func (ctx *compileCtx) compileUnary(expr ast.Expr, op Op) error {
	err := ctx.compileAST(expr)
	if err != nil {
		return err
	}
	ctx.pushOp(op)
	return nil
}

func (ctx *compileCtx) compileTypeAssert(node *ast.TypeAssertExpr) error {
	err := ctx.compileAST(node.X)
	if err != nil {
		return err
	}
	// Accept .(data) as a type assertion that always succeeds, so that users
	// can access the data field of an interface without actually having to
	// type the concrete type.
	if idtyp, isident := node.Type.(*ast.Ident); !isident || idtyp.Name != "data" {
		typ, err := ctx.FindTypeExpr(node.Type)
		if err != nil {
			return err
		}
		ctx.pushOp(&TypeAssert{typ, node})
		return nil
	}
	ctx.pushOp(&TypeAssert{nil, node})
	return nil
}

func (ctx *compileCtx) compileBinary(a, b ast.Expr, sop *Jump, op Op) error {
	err := ctx.compileAST(a)
	if err != nil {
		return err
	}
	if sop != nil {
		ctx.pushOp(sop)
	}
	err = ctx.compileAST(b)
	if err != nil {
		return err
	}
	ctx.pushOp(op)
	return nil
}

func (ctx *compileCtx) compileReslice(node *ast.SliceExpr) error {
	err := ctx.compileAST(node.X)
	if err != nil {
		return err
	}

	hasHigh := false
	if node.High != nil {
		hasHigh = true
		err = ctx.compileAST(node.High)
		if err != nil {
			return err
		}
	}

	if node.Low != nil {
		err = ctx.compileAST(node.Low)
		if err != nil {
			return err
		}
	} else {
		ctx.pushOp(&PushConst{constant.MakeInt64(0)})
	}

	ctx.pushOp(&Reslice{Node: node, HasHigh: hasHigh})
	return nil
}

func (ctx *compileCtx) compileFunctionCall(node *ast.CallExpr) error {
	if fnnode, ok := node.Fun.(*ast.Ident); ok {
		if ctx.HasBuiltin(fnnode.Name) {
			return ctx.compileBuiltinCall(fnnode.Name, node.Args)
		}
	}
	if !ctx.allowCalls {
		return ErrFuncCallNotAllowed
	}

	id := ctx.curCall
	ctx.curCall++

	if ctx.flags&HasDebugPinner != 0 {
		return ctx.compileFunctionCallNew(node, id)
	}

	return ctx.compileFunctionCallOld(node, id)
}

// compileFunctionCallOld compiles a function call when runtime.debugPinner is
// not available in the target.
func (ctx *compileCtx) compileFunctionCallOld(node *ast.CallExpr, id int) error {
	oldAllowCalls := ctx.allowCalls
	oldOps := ctx.ops
	ctx.allowCalls = false
	err := ctx.compileAST(node.Fun)
	ctx.allowCalls = oldAllowCalls
	hasFunc := false
	if err != nil {
		ctx.ops = oldOps
		if err != ErrFuncCallNotAllowed {
			return err
		}
	} else {
		hasFunc = true
	}
	ctx.pushOp(&CallInjectionStart{HasFunc: hasFunc, id: id, Node: node})

	// CallInjectionStart pushes true on the stack if it needs the function argument re-evaluated
	var jmpif *Jump
	if hasFunc {
		jmpif = &Jump{When: JumpIfFalse, Pop: true}
		ctx.pushOp(jmpif)
	}
	ctx.pushOp(&Pop{})
	err = ctx.compileAST(node.Fun)
	if err != nil {
		return err
	}
	if jmpif != nil {
		jmpif.Target = len(ctx.ops)
	}

	ctx.pushOp(&CallInjectionSetTarget{id: id})

	for i, arg := range node.Args {
		err := ctx.compileAST(arg)
		if err != nil {
			return fmt.Errorf("error evaluating %q as argument %d in function %s: %v", ExprToString(arg), i+1, ExprToString(node.Fun), err)
		}
		if isStringLiteral(arg) {
			ctx.compileAllocLiteralString()
		}
		ctx.pushOp(&CallInjectionCopyArg{id: id, ArgNum: i, ArgExpr: arg})
	}

	ctx.pushOp(&CallInjectionComplete{id: id})

	return nil
}

// compileFunctionCallNew compiles a function call when runtime.debugPinner
// is available in the target.
func (ctx *compileCtx) compileFunctionCallNew(node *ast.CallExpr, id int) error {
	ctx.compileGetDebugPinner()

	err := ctx.compileAST(node.Fun)
	if err != nil {
		return err
	}

	for i, arg := range node.Args {
		err := ctx.compileAST(arg)
		if isStringLiteral(arg) {
			ctx.compileAllocLiteralString()
		}
		if err != nil {
			return fmt.Errorf("error evaluating %q as argument %d in function %s: %v", ExprToString(arg), i+1, ExprToString(node.Fun), err)
		}
	}

	ctx.pushOp(&Roll{len(node.Args)})
	ctx.pushOp(&CallInjectionStart{HasFunc: true, id: id, Node: node})
	ctx.pushOp(&Pop{})
	ctx.pushOp(&CallInjectionSetTarget{id: id})

	for i := len(node.Args) - 1; i >= 0; i-- {
		arg := node.Args[i]
		ctx.pushOp(&CallInjectionCopyArg{id: id, ArgNum: i, ArgExpr: arg})
	}

	ctx.pushOp(&CallInjectionComplete{id: id, DoPinning: true})

	ctx.compilePinningLoop(id)

	return nil
}

func (ctx *compileCtx) compilePinningLoop(id int) {
	loopStart := len(ctx.ops)
	jmp := &Jump{When: JumpIfPinningDone}
	ctx.pushOp(jmp)
	ctx.pushOp(&PushPinAddress{})
	ctx.compileSpecialCall("runtime.(*Pinner).Pin", []ast.Expr{
		&ast.Ident{Name: "debugPinner"},
		&ast.Ident{Name: "pinAddress"},
	}, []Op{
		&PushDebugPinner{},
		nil,
	}, false)
	ctx.pushOp(&Pop{})
	ctx.pushOp(&Jump{When: JumpAlways, Target: loopStart})
	jmp.Target = len(ctx.ops)
	ctx.pushOp(&CallInjectionComplete2{id: id})
}

func Listing(depth []int, ops []Op) string {
	if depth == nil {
		depth = make([]int, len(ops)+1)
	}
	buf := new(strings.Builder)
	for i, op := range ops {
		fmt.Fprintf(buf, " %3d  (%2d->%2d) %#v\n", i, depth[i], depth[i+1], op)
	}
	return buf.String()
}

func isStringLiteral(expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return expr.Kind == token.STRING
	case *ast.BinaryExpr:
		if expr.Op == token.ADD {
			return isStringLiteral(expr.X) && isStringLiteral(expr.Y)
		}
	case *ast.ParenExpr:
		return isStringLiteral(expr.X)
	}
	return false
}

func removeParen(n ast.Expr) ast.Expr {
	for {
		p, ok := n.(*ast.ParenExpr)
		if !ok {
			break
		}
		n = p.X
	}
	return n
}

func ExprToString(t ast.Expr) string {
	var buf bytes.Buffer
	printer.Fprint(&buf, token.NewFileSet(), t)
	return buf.String()
}

func ResolveTypedef(typ godwarf.Type) godwarf.Type {
	for {
		switch tt := typ.(type) {
		case *godwarf.TypedefType:
			typ = tt.Type
		case *godwarf.QualType:
			typ = tt.Type
		default:
			return typ
		}
	}
}
