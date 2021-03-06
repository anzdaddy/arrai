package syntax

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/anz-bank/pkg/log"
	"github.com/arr-ai/wbnf/ast"

	"github.com/arr-ai/arrai/rel"
	"github.com/arr-ai/wbnf/parser"
)

// type noParseType struct{}

// type parseFunc func(v interface{}) (rel.Expr, error)

// func (*noParseType) Error() string {
// 	return "No parse"
// }

// var noParse = &noParseType{}

const NoPath = "\000"

const exprTag = "expr"

var loggingOnce sync.Once

// Compile compiles source string.
func Compile(filepath, source string) (_ rel.Expr, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("error compiling %q: %v", filepath, r)
			}
		}
	}()
	return MustCompile(filepath, source), nil
}

func MustCompile(filePath, source string) rel.Expr {
	dirpath := "."
	if filePath != "" {
		if filePath == NoPath {
			dirpath = NoPath
		} else {
			dirpath = path.Dir(filePath)
		}
	}
	pc := ParseContext{SourceDir: dirpath}
	if !filepath.IsAbs(filePath) {
		var err error
		filePath, err = filepath.Rel(".", filePath)
		if err != nil {
			panic(err)
		}
	}
	ast, err := pc.Parse(parser.NewScannerWithFilename(source, filePath))
	if err != nil {
		panic(err)
	}
	return pc.CompileExpr(ast)
}

func (pc ParseContext) CompileExpr(b ast.Branch) rel.Expr {
	// Note: please make sure if it is necessary to add new syntax name before `expr`.
	name, c := which(b,
		"amp", "arrow", "let", "unop", "binop", "compare", "rbinop", "if", "get",
		"tail_op", "postfix", "touch", "get", "rel", "set", "dict", "array", "bytes",
		"embed", "op", "fn", "pkg", "tuple", "xstr", "IDENT", "STR", "NUM", "CHAR",
		"cond", exprTag,
	)
	if c == nil {
		panic(fmt.Errorf("misshapen node AST: %v", b))
	}
	switch name {
	case "amp", "arrow":
		return pc.compileArrow(b, name, c)
	case "let":
		return pc.compileLet(c)
	case "unop":
		return pc.compileUnop(b, c)
	case "binop":
		return pc.compileBinop(b, c)
	case "compare":
		return pc.compileCompare(b, c)
	case "rbinop":
		return pc.compileRbinop(b, c)
	case "if":
		return pc.compileIf(b, c)
	case "cond":
		return pc.compileCond(c)
	case "postfix", "touch":
		return pc.compilePostfixAndTouch(b, c)
	case "get", "tail_op":
		return pc.compileCallGet(b)
	case "rel":
		return pc.compileRelation(b, c)
	case "set":
		return pc.compileSet(b, c)
	case "dict":
		return pc.compileDict(b, c)
	case "array":
		return pc.compileArray(b, c)
	case "bytes":
		return pc.compileBytes(b, c)
	case "embed":
		return pc.compileMacro(b)
	case "fn":
		return pc.compileFunction(b)
	case "pkg":
		return pc.compilePackage(b, c)
	case "tuple":
		return pc.compileTuple(b, c)
	case "IDENT":
		return pc.compileIdent(c)
	case "STR":
		return pc.compileString(c)
	case "xstr":
		return pc.compileExpandableString(b, c)
	case "NUM":
		return pc.compileNumber(c)
	case "CHAR":
		return pc.compileChar(c)
	case exprTag:
		if result := pc.compileExpr(b, c); result != nil {
			return result
		}
	}
	panic(fmt.Errorf("unhandled node: %v", b))
}

func (pc ParseContext) compilePattern(b ast.Branch) rel.Pattern {
	if ptn := b.One("pattern"); ptn != nil {
		return pc.compilePattern(ptn.(ast.Branch))
	}
	if arr := b.One("array"); arr != nil {
		return pc.compileArrayPattern(arr.(ast.Branch))
	}
	if tuple := b.One("tuple"); tuple != nil {
		return pc.compileTuplePattern(tuple.(ast.Branch))
	}
	if dict := b.One("dict"); dict != nil {
		return pc.compileDictPattern(dict.(ast.Branch))
	}
	if set := b.One("set"); set != nil {
		return pc.compileSetPattern(set.(ast.Branch))
	}
	if extra := b.One("extra"); extra != nil {
		return pc.compileExtraElementPattern(extra.(ast.Branch))
	}
	if expr := b.Many("exprpattern"); expr != nil {
		var elements []rel.Expr
		for _, e := range expr {
			expr := pc.CompileExpr(e.(ast.Branch))
			elements = append(elements, expr)
		}
		if len(elements) > 0 {
			return rel.NewExprsPattern(elements...)
		}
	}

	return rel.NewExprPattern(pc.CompileExpr(b))
}

func (pc ParseContext) compileExtraElementPattern(b ast.Branch) rel.Pattern {
	var ident string
	if id := b.One("ident"); id != nil {
		ident = id.Scanner().String()
	}
	return rel.NewExtraElementPattern(ident)
}

func (pc ParseContext) compilePatterns(exprs ...ast.Node) []rel.Pattern {
	result := make([]rel.Pattern, 0, len(exprs))
	for _, expr := range exprs {
		result = append(result, pc.compilePattern(expr.(ast.Branch)))
	}
	return result
}

func (pc ParseContext) compileSparsePatterns(b ast.Branch) []rel.FallbackPattern {
	var nodes []ast.Node
	if firstItem, exists := b["first_item"]; exists {
		nodes = []ast.Node{firstItem.(ast.One).Node}
		if items, exists := b["item"]; exists {
			for _, i := range items.(ast.Many) {
				nodes = append(nodes, i)
			}
		}
	}
	result := make([]rel.FallbackPattern, 0, len(nodes))
	for _, expr := range nodes {
		if expr.One("empty") != nil {
			result = append(result, rel.NewFallbackPattern(nil, nil))
			continue
		}
		ptn := pc.compilePattern(expr.(ast.Branch))
		if fall := expr.One("fall"); fall != nil {
			fallback := pc.CompileExpr(fall.(ast.Branch))
			result = append(result, rel.NewFallbackPattern(ptn, fallback))
			continue
		}
		result = append(result, rel.NewFallbackPattern(ptn, nil))
	}
	return result
}

func (pc ParseContext) compileArrayPattern(b ast.Branch) rel.Pattern {
	return rel.NewArrayPattern(pc.compileSparsePatterns(b)...)
}

func (pc ParseContext) compileTuplePattern(b ast.Branch) rel.Pattern {
	if pairs := b.Many("pairs"); pairs != nil {
		attrs := make([]rel.TuplePatternAttr, 0, len(pairs))
		for _, pair := range pairs {
			var k string
			var v rel.Pattern

			if extra := pair.One("extra"); extra != nil {
				v = pc.compilePattern(pair.(ast.Branch))
				attrs = append(attrs, rel.NewTuplePatternAttr(k, rel.NewFallbackPattern(v, nil)))
			} else {
				v = pc.compilePattern(pair.One("v").(ast.Branch))
				if name := pair.One("name"); name != nil {
					k = parseName(name.(ast.Branch))
				} else {
					k = v.String()
				}

				tail := pair.One("tail")
				fall := pair.One("v").One("fall")
				if fall == nil {
					attrs = append(attrs, rel.NewTuplePatternAttr(k, rel.NewFallbackPattern(v, nil)))
				} else if tail != nil && fall != nil {
					attrs = append(attrs, rel.NewTuplePatternAttr(k, rel.NewFallbackPattern(v, pc.CompileExpr(fall.(ast.Branch)))))
				} else {
					panic("fallback item does not match")
				}
			}
		}
		return rel.NewTuplePattern(attrs...)
	}
	return rel.NewTuplePattern()
}

func (pc ParseContext) compileDictPattern(b ast.Branch) rel.Pattern {
	if pairs := b.Many("pairs"); pairs != nil {
		entryPtns := make([]rel.DictPatternEntry, 0, len(pairs))
		for _, pair := range pairs {
			if extra := pair.One("extra"); extra != nil {
				p := pc.compileExtraElementPattern(extra.(ast.Branch))
				entryPtns = append(entryPtns, rel.NewDictPatternEntry(nil, rel.NewFallbackPattern(p, nil)))
				continue
			}
			key := pair.One("key")
			value := pair.One("value")
			keyExpr := pc.CompileExpr(key.(ast.Branch))
			valuePtn := pc.compilePattern(value.(ast.Branch))

			tail := key.One("tail")
			fall := value.One("fall")
			if fall == nil {
				entryPtns = append(entryPtns, rel.NewDictPatternEntry(keyExpr, rel.NewFallbackPattern(valuePtn, nil)))
			} else if tail != nil && fall != nil {
				entryPtns = append(entryPtns, rel.NewDictPatternEntry(keyExpr,
					rel.NewFallbackPattern(valuePtn, pc.CompileExpr(fall.(ast.Branch)))))
			} else {
				panic("fallback item does not match")
			}
		}
		return rel.NewDictPattern(entryPtns...)
	}
	return rel.NewDictPattern()
}

func (pc ParseContext) compileSetPattern(b ast.Branch) rel.Pattern {
	if elts := b["elt"]; elts != nil {
		return rel.NewSetPattern(pc.compilePatterns(elts.(ast.Many)...)...)
	}
	return rel.NewSetPattern()
}

func (pc ParseContext) compileArrow(b ast.Branch, name string, c ast.Children) rel.Expr {
	expr := pc.CompileExpr(b[exprTag].(ast.One).Node.(ast.Branch))
	source := c.Scanner()
	if arrows, has := b["arrow"]; has {
		for _, arrow := range arrows.(ast.Many) {
			branch := arrow.(ast.Branch)
			part, d := which(branch, "nest", "unnest", "ARROW", "binding", "FILTER")
			switch part {
			case "nest":
				expr = parseNest(expr, branch["nest"].(ast.One).Node.(ast.Branch))
			case "unnest":
				panic("unfinished")
			case "ARROW":
				op := d.(ast.One).Node.One("").(ast.Leaf).Scanner()
				f := binops[op.String()]
				expr = f(b.Scanner(), expr, pc.CompileExpr(arrow.(ast.Branch)[exprTag].(ast.One).Node.(ast.Branch)))
			case "binding":
				rhs := pc.CompileExpr(arrow.(ast.Branch)[exprTag].(ast.One).Node.(ast.Branch))
				if pattern := arrow.One("pattern"); pattern != nil {
					p := pc.compilePattern(pattern.(ast.Branch))
					rhs = rel.NewFunction(source, p, rhs)
				}
				expr = binops["->"](source, expr, rhs)
			case "FILTER":
				pred := pc.CompileExpr(arrow.(ast.Branch))
				lhs := rel.NewWhereExpr(source, expr, pred)
				expr = rel.NewDArrowExpr(source, lhs, pred)
			}
		}
	}
	if name == "amp" {
		for range c.(ast.Many) {
			expr = rel.NewFunction(source, rel.NewExprPattern(rel.NewIdentExpr(source, "-")), expr)
		}
	}
	return expr
}

// let PATTERN                     = EXPR1;      EXPR2
// let c.(ast.One).Node.One("...") = expr(lhs);  rhs
// EXPR1 -> \PATTERN EXPR2
func (pc ParseContext) compileLet(c ast.Children) rel.Expr {
	exprs := c.(ast.One).Node.Many(exprTag)
	expr := pc.CompileExpr(exprs[0].(ast.Branch))
	rhs := pc.CompileExpr(exprs[1].(ast.Branch))
	source := c.Scanner()

	p := pc.compilePattern(c.(ast.One).Node.(ast.Branch))
	rhs = rel.NewFunction(source, p, rhs)

	if c.(ast.One).Node.One("rec") != nil {
		fix, fixt := FixFuncs()
		name := p.(rel.ExprPattern).Expr
		expr = rel.NewRecursionExpr(c.Scanner(), name, expr, fix, fixt)
	}

	return binops["->"](source, expr, rhs)
}

func (pc ParseContext) compileUnop(b ast.Branch, c ast.Children) rel.Expr {
	ops := c.(ast.Many)
	result := pc.CompileExpr(b.One(exprTag).(ast.Branch))
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i].One("").(ast.Leaf).Scanner()
		f := unops[op.String()]
		source, err := parser.MergeScanners(op, result.Source())
		if err != nil {
			// TODO: Figure out why some exprs don't have usable sources (could be native funcs).
			source = op
		}
		result = f(source, result)
	}
	return result
}

func (pc ParseContext) compileBinop(b ast.Branch, c ast.Children) rel.Expr {
	ops := c.(ast.Many)
	args := b.Many(exprTag)
	result := pc.CompileExpr(args[0].(ast.Branch))
	for i, arg := range args[1:] {
		op := ops[i].One("").(ast.Leaf).Scanner()
		f := binops[op.String()]
		rhs := pc.CompileExpr(arg.(ast.Branch))
		source, err := parser.MergeScanners(op, result.Source(), rhs.Source())
		if err != nil {
			// TODO: Figure out why some exprs don't have usable sources (could be native funcs).
			source = op
		}
		result = f(source, result, rhs)
	}
	return result
}

func (pc ParseContext) compileCompare(b ast.Branch, c ast.Children) rel.Expr {
	args := b.Many(exprTag)
	argExprs := make([]rel.Expr, 0, len(args))
	comps := make([]rel.CompareFunc, 0, len(args))

	ops := c.(ast.Many)
	opStrs := make([]string, 0, len(ops))

	argExprs = append(argExprs, pc.CompileExpr(args[0].(ast.Branch)))
	for i, arg := range args[1:] {
		op := ops[i].One("").(ast.Leaf).Scanner().String()

		argExprs = append(argExprs, pc.CompileExpr(arg.(ast.Branch)))
		comps = append(comps, compareOps[op])

		opStrs = append(opStrs, op)
	}
	scanner, err := parser.MergeScanners(argExprs[0].Source(), argExprs[len(argExprs)-1].Source())
	if err != nil {
		panic(err)
	}
	return rel.NewCompareExpr(scanner, argExprs, comps, opStrs)
}

func (pc ParseContext) compileRbinop(b ast.Branch, c ast.Children) rel.Expr {
	ops := c.(ast.Many)
	args := b[exprTag].(ast.Many)
	result := pc.CompileExpr(args[len(args)-1].(ast.Branch))
	for i := len(args) - 2; i >= 0; i-- {
		op := ops[i].One("").(ast.Leaf).Scanner()
		f, has := binops[op.String()]
		if !has {
			panic("rbinop %q not found")
		}
		result = f(op, pc.CompileExpr(args[i].(ast.Branch)), result)
	}
	return result
}

func (pc ParseContext) compileIf(b ast.Branch, c ast.Children) rel.Expr {
	loggingOnce.Do(func() {
		log.Error(context.Background(),
			errors.New("operator if is deprecated and will be removed soon, please use operator cond instead. "+
				"Operator cond sample: let a = cond {2 > 1: 1, 2 > 3: 2, _: 3}"))
	})

	result := pc.CompileExpr(b.One(exprTag).(ast.Branch))
	source := result.Source()
	for _, ifelse := range c.(ast.Many) {
		t := pc.CompileExpr(ifelse.One("t").(ast.Branch))
		var f rel.Expr = rel.None
		if fNode := ifelse.One("f"); fNode != nil {
			f = pc.CompileExpr(fNode.(ast.Branch))
		}
		result = rel.NewIfElseExpr(source, result, t, f)
	}
	return result
}

func (pc ParseContext) compileCond(c ast.Children) rel.Expr {
	if controlVar := c.(ast.One).Node.(ast.Branch)["controlVar"]; controlVar != nil {
		return pc.compileCondWithControlVar(c)
	}
	return pc.compileCondWithoutControlVar(c)
}

func (pc ParseContext) compileCondWithControlVar(c ast.Children) rel.Expr {
	conditions := pc.compileCondElements(c.(ast.One).Node.(ast.Branch)["condition"].(ast.Many)...)
	values := pc.compileCondExprs(c.(ast.One).Node.(ast.Branch)["value"].(ast.Many)...)

	if len(conditions) != len(values) {
		panic("mismatch between conditions and values")
	}

	conditionPairs := []rel.PatternExprPair{}
	for i, condition := range conditions {
		conditionPairs = append(conditionPairs, rel.NewPatternExprPair(condition, values[i]))
	}

	controlVar := c.(ast.One).Node.(ast.Branch)["controlVar"]
	return rel.NewCondPatternControlVarExpr(c.(ast.One).Node.Scanner(),
		pc.CompileExpr(controlVar.(ast.One).Node.(ast.Branch)),
		conditionPairs...)
}

func (pc ParseContext) compileCondElements(elements ...ast.Node) []rel.Pattern {
	result := make([]rel.Pattern, 0, len(elements))
	for _, element := range elements {
		name, c := which(element.(ast.Branch), "pattern")
		if c == nil {
			panic(fmt.Errorf("misshapen node AST: %v", element.(ast.Branch)))
		}

		if name == "pattern" {
			pattern := pc.compilePattern(element.(ast.Branch))
			if pattern != nil {
				result = append(result, pattern)
			}
		}
	}

	return result
}

func (pc ParseContext) compileCondWithoutControlVar(c ast.Children) rel.Expr {
	var result rel.Expr
	entryExprs := pc.compileDictEntryExprs(c.(ast.One).Node.(ast.Branch))
	if entryExprs != nil {
		// Generates type DictExpr always to make sure it is easy to do Eval, only process type DictExpr.
		result = rel.NewDictExpr(c.(ast.One).Node.Scanner(), false, true, entryExprs...)
	} else {
		result = rel.NewDict(false)
	}

	// Note, the default case `_:expr` which can match anything is parsed to condition/value pairs by current syntax.
	return rel.NewCondExpr(c.(ast.One).Node.Scanner(), result)
}

func (pc ParseContext) compilePostfixAndTouch(b ast.Branch, c ast.Children) rel.Expr {
	if _, has := b["touch"]; has {
		panic("unfinished")
	}
	switch c.Scanner().String() {
	case "count":
		return rel.NewCountExpr(b.Scanner(), pc.CompileExpr(b.One(exprTag).(ast.Branch)))
	case "single":
		return rel.NewSingleExpr(b.Scanner(), pc.CompileExpr(b.One(exprTag).(ast.Branch)))
	default:
		panic("wat?")
	}

	// touch -> ("->*" ("&"? IDENT | STR))+ "(" expr:"," ","? ")";
	// result := p.parseExpr(b.One(exprTag).(ast.Branch))
}

func (pc ParseContext) compileCallGet(b ast.Branch) rel.Expr {
	var result rel.Expr
	if expr := b.One(exprTag); expr != nil {
		result = pc.CompileExpr(expr.(ast.Branch))
	} else {
		get := b.One("get")
		dot := get.One("dot")
		result = pc.compileGet(rel.NewDotIdent(dot.Scanner()), get)
	}
	for _, part := range b.Many("tail_op") {
		if safe := part.One("safe_tail"); safe != nil {
			result = pc.compileSafeTails(result, part.One("safe_tail"))
		} else {
			result = pc.compileTail(result, part.One("tail"))
		}
	}
	return result
}

func (pc ParseContext) compileTail(base rel.Expr, tail ast.Node) rel.Expr {
	if tail != nil {
		if call := tail.One("call"); call != nil {
			args := call.Many("arg")
			exprs := make([]ast.Node, 0, len(args))
			for _, arg := range args {
				exprs = append(exprs, arg.One(exprTag))
			}
			for _, arg := range pc.compileExprs(exprs...) {
				base = rel.NewCallExpr(handleAccessScanners(base.Source(), call.Scanner()), base, arg)
			}
		}
		base = pc.compileGet(base, tail.One("get"))
	}
	return base
}

func (pc ParseContext) compileTailFunc(tail ast.Node) rel.SafeTailCallback {
	if tail != nil {
		if call := tail.One("call"); call != nil {
			args := call.Many("arg")
			exprs := make([]ast.Node, 0, len(args))
			for _, arg := range args {
				exprs = append(exprs, arg.One("expr"))
			}
			compiledExprs := pc.compileExprs(exprs...)
			return func(v rel.Value, local rel.Scope) (rel.Value, error) {
				for _, arg := range compiledExprs {
					a, err := arg.Eval(local)
					if err != nil {
						return nil, err
					}
					//TODO: scanner won't highlight calls properly in safe call
					set, is := v.(rel.Set)
					if !is {
						return nil, fmt.Errorf("not a set: %v", v)
					}
					v, err = rel.SetCall(set, a)
					if err != nil {
						return nil, err
					}
				}
				return v, nil
			}
		}
		if get := tail.One("get"); get != nil {
			var scanner parser.Scanner
			var attr string
			if ident := get.One("IDENT"); ident != nil {
				scanner = ident.One("").(ast.Leaf).Scanner()
				attr = scanner.String()
			}
			if str := get.One("STR"); str != nil {
				scanner = str.One("").Scanner()
				attr = parseArraiString(scanner.String())
			}
			return func(v rel.Value, local rel.Scope) (rel.Value, error) {
				return rel.NewDotExpr(handleAccessScanners(v.Source(), scanner), v, attr).Eval(local)
			}
		}
	}
	panic("no tail")
}

func (pc ParseContext) compileGet(base rel.Expr, get ast.Node) rel.Expr {
	if get != nil {
		if names := get.One("names"); names != nil {
			inverse := get.One("") != nil
			attrs := parseNames(names.(ast.Branch))
			return rel.NewTupleProjectExpr(
				handleAccessScanners(base.Source(), names.Scanner()),
				base, inverse, attrs,
			)
		}

		var scanner parser.Scanner
		var attr string
		if ident := get.One("IDENT"); ident != nil {
			scanner = ident.One("").(ast.Leaf).Scanner()
			attr = scanner.String()
		}
		if str := get.One("STR"); str != nil {
			scanner = str.One("").Scanner()
			attr = parseArraiString(scanner.String())
		}

		base = rel.NewDotExpr(handleAccessScanners(base.Source(), scanner), base, attr)
	}
	return base
}

func (pc ParseContext) compileSafeTails(base rel.Expr, tail ast.Node) rel.Expr {
	if tail != nil {
		firstSafe := tail.One("first_safe").One("tail")
		safeCallback := func(tailFunc rel.SafeTailCallback) rel.SafeTailCallback {
			return func(v rel.Value, local rel.Scope) (rel.Value, error) {
				val, err := tailFunc(v, local)
				if err != nil {
					switch e := err.(type) {
					case rel.NoReturnError:
						return nil, nil
					case rel.ContextErr:
						if _, isMissingAttrError := e.NextErr().(rel.MissingAttrError); isMissingAttrError {
							return nil, nil
						}
					}
					return nil, err
				}
				return val, nil
			}
		}

		exprStates := []rel.SafeTailCallback{safeCallback(pc.compileTailFunc(firstSafe))}
		fallback := pc.CompileExpr(tail.One("fall").(ast.Branch))

		for _, o := range tail.Many("ops") {
			if safeTail := o.One("safe"); safeTail != nil {
				exprStates = append(exprStates, safeCallback(pc.compileTailFunc(safeTail.One("tail"))))
			} else if tail := o.One("tail"); tail != nil {
				exprStates = append(exprStates, pc.compileTailFunc(tail))
			} else {
				panic("wat")
			}
		}

		return rel.NewSafeTailExpr(tail.Scanner(), fallback, base, exprStates)
	}
	//TODO: panic?
	return base
}

func handleAccessScanners(base, access parser.Scanner) parser.Scanner {
	if len(base.String()) == 0 {
		return access
	}
	// handles .a
	if base.String() == "." {
		return *access.Skip(-1)
	}
	scanner, err := parser.MergeScanners(base, access)
	if err != nil {
		panic(err)
	}
	return scanner
}

func (pc ParseContext) compileRelation(b ast.Branch, c ast.Children) rel.Expr {
	names := parseNames(c.(ast.One).Node.(ast.Branch)["names"].(ast.One).Node.(ast.Branch))
	tuples := c.(ast.One).Node.(ast.Branch)["tuple"].(ast.Many)
	tupleExprs := make([][]rel.Expr, 0, len(tuples))
	for _, tuple := range tuples {
		tupleExprs = append(tupleExprs, pc.compileExprs(tuple.(ast.Branch)["v"].(ast.Many)...))
	}
	result, err := rel.NewRelationExpr(
		delimsScanner(b),
		names,
		tupleExprs...,
	)
	if err != nil {
		panic(err)
	}
	return result
}

func (pc ParseContext) compileSet(b ast.Branch, c ast.Children) rel.Expr {
	scanner := delimsScanner(b)
	if elts := c.(ast.One).Node.(ast.Branch)["elt"]; elts != nil {
		return rel.NewSetExpr(scanner, pc.compileExprs(elts.(ast.Many)...)...)
	}
	return rel.NewLiteralExpr(scanner, rel.NewSet())
}

func (pc ParseContext) compileDict(b ast.Branch, c ast.Children) rel.Expr {
	scanner := delimsScanner(b)
	entryExprs := pc.compileDictEntryExprs(c.(ast.One).Node.(ast.Branch))
	if entryExprs != nil {
		return rel.NewDictExpr(scanner, false, false, entryExprs...)
	}

	return rel.NewLiteralExpr(scanner, rel.NewDict(false))
}

func (pc ParseContext) compileDictEntryExprs(b ast.Branch) []rel.DictEntryTupleExpr {
	if pairs := b.Many("pairs"); pairs != nil {
		entryExprs := make([]rel.DictEntryTupleExpr, 0, len(pairs))
		for _, pair := range pairs {
			key := pair.One("key")
			value := pair.One("value")
			keyExpr := pc.CompileExpr(key.(ast.Branch))
			valueExpr := pc.CompileExpr(value.(ast.Branch))
			entryExprs = append(entryExprs, rel.NewDictEntryTupleExpr(pair.Scanner(), keyExpr, valueExpr))
		}
		return entryExprs
	}
	return nil
}

func (pc ParseContext) compileArray(b ast.Branch, c ast.Children) rel.Expr {
	scanner := delimsScanner(b)
	if exprs := pc.compileSparseItems(c); len(exprs) > 0 {
		return rel.NewArrayExpr(scanner, exprs...)
	}
	return rel.NewLiteralExpr(scanner, rel.NewArray())
}

func (pc ParseContext) compileBytes(b ast.Branch, c ast.Children) rel.Expr {
	if items := c.(ast.One).Node.(ast.Branch)["item"]; items != nil {
		//TODO: support sparse bytes
		return rel.NewBytesExpr(delimsScanner(b), pc.compileExprs(items.(ast.Many)...)...)
	}
	return rel.NewBytes([]byte{})
}

func (pc ParseContext) compileExprs(exprs ...ast.Node) []rel.Expr {
	result := make([]rel.Expr, 0, len(exprs))
	for _, expr := range exprs {
		result = append(result, pc.CompileExpr(expr.(ast.Branch)))
	}
	return result
}

func (pc ParseContext) compileSparseItems(c ast.Children) []rel.Expr {
	var nodes []ast.Node
	if firstItem := c.(ast.One).Node.One("first_item"); firstItem != nil {
		nodes = []ast.Node{firstItem}
		if items := c.(ast.One).Node.Many("item"); items != nil {
			nodes = append(nodes, items...)
		}
	}
	result := make([]rel.Expr, 0, len(nodes))
	for _, expr := range nodes {
		if expr.One("empty") != nil {
			result = append(result, nil)
			continue
		}
		result = append(result, pc.CompileExpr(expr.(ast.Branch)))
	}
	return result
}

// compileCondExprs parses conditons/keys and values expressions for syntax `cond`.
func (pc ParseContext) compileCondExprs(exprs ...ast.Node) []rel.Expr {
	result := make([]rel.Expr, 0, len(exprs))
	for _, expr := range exprs {
		var exprResult rel.Expr

		name, c := which(expr.(ast.Branch), exprTag)
		if c == nil {
			panic(fmt.Errorf("misshapen node AST: %v", expr.(ast.Branch)))
		}

		if name == exprTag {
			switch c := c.(type) {
			case ast.One:
				exprResult = pc.CompileExpr(c.Node.(ast.Branch))
			case ast.Many:
				if len(c) == 1 {
					exprResult = pc.CompileExpr(c[0].(ast.Branch))
				} else {
					var elements []rel.Expr
					for _, e := range c {
						expr := pc.CompileExpr(e.(ast.Branch))
						elements = append(elements, expr)
					}
					exprResult = rel.NewArrayExpr(c.Scanner(), elements...)
				}
			}
		}

		if exprResult != nil {
			result = append(result, exprResult)
		}
	}
	return result
}

func (pc ParseContext) compileFunction(b ast.Branch) rel.Expr {
	p := pc.compilePattern(b)
	expr := pc.CompileExpr(b.One(exprTag).(ast.Branch))
	return rel.NewFunction(b.Scanner(), p, expr)
}

func (pc ParseContext) compileMacro(b ast.Branch) rel.Expr {
	childast := b.One("embed").One("subgrammar").One("ast")
	if value := childast.One("value"); value != nil {
		return value.(MacroValue).SubExpr()
	}
	return rel.ASTNodeToValue(childast)
}

func (pc ParseContext) compilePackage(b ast.Branch, c ast.Children) rel.Expr {
	imp := b.One("import").Scanner()
	pkg := c.(ast.One).Node.(ast.Branch)
	if std, has := pkg["std"]; has {
		ident := std.(ast.One).Node.One("IDENT").One("")
		pkgName := ident.(ast.Leaf).Scanner()
		scanner, err := parser.MergeScanners(imp, pkgName)
		if err != nil {
			panic(err)
		}
		return NewPackageExpr(pkgName, rel.NewDotExpr(scanner, rel.NewIdentExpr(imp, imp.String()), pkgName.String()))
	}

	if pkgpath := pkg.One("PKGPATH"); pkgpath != nil {
		scanner := pkgpath.One("").(ast.Leaf).Scanner()
		name := scanner.String()
		if strings.HasPrefix(name, "/") {
			filepath := strings.Trim(name, "/")
			fromRoot := pkg["dot"] == nil
			if pc.SourceDir == "" {
				panic(fmt.Errorf("local import %q invalid; no local context", name))
			}
			return rel.NewCallExpr(scanner,
				NewPackageExpr(scanner, importLocalFile(fromRoot)),
				rel.NewString([]rune(path.Join(pc.SourceDir, filepath))),
			)
		}
		return rel.NewCallExpr(scanner, NewPackageExpr(scanner, importExternalContent()), rel.NewString([]rune(name)))
	}
	panic("malformed package AST")
}

func (pc ParseContext) compileTuple(b ast.Branch, c ast.Children) rel.Expr {
	scanner := delimsScanner(b)
	if pairs := c.(ast.One).Node.Many("pairs"); pairs != nil {
		attrs := make([]rel.AttrExpr, 0, len(pairs))
		for _, pair := range pairs {
			var k string
			v := pc.CompileExpr(pair.One("v").(ast.Branch))
			if name := pair.One("name"); name != nil {
				k = parseName(name.(ast.Branch))
			} else {
				switch v := v.(type) {
				case *rel.DotExpr:
					k = v.Attr()
				case rel.IdentExpr:
					k = v.Ident()
				default:
					panic(fmt.Errorf("unnamed attr expression must be name or end in .name: %T(%[1]v)", v))
				}
			}
			scanner := pair.One("v").(ast.Branch).Scanner()
			if pair.One("rec") != nil {
				fix, fixt := FixFuncs()
				v = rel.NewRecursionExpr(
					scanner,
					rel.NewIdentExpr(pair.One("name").Scanner(), k),
					v, fix, fixt,
				)
			}
			attr, err := rel.NewAttrExpr(scanner, k, v)
			if err != nil {
				panic(err)
			}
			attrs = append(attrs, attr)
		}
		return rel.NewTupleExpr(scanner, attrs...)
	}
	return rel.NewLiteralExpr(scanner, rel.EmptyTuple)
}

func delimsScanner(b ast.Branch) parser.Scanner {
	result, err := parser.MergeScanners(b.One("odelim").Scanner(), b.One("cdelim").Scanner())
	if err != nil {
		panic(err)
	}
	return result
}

func (pc ParseContext) compileIdent(c ast.Children) rel.Expr {
	scanner := c.(ast.One).Node.One("").Scanner()
	var value rel.Value
	switch scanner.String() {
	case "true":
		value = rel.True
	case "false":
		value = rel.False
	default:
		return rel.NewIdentExpr(scanner, scanner.String())
	}
	return rel.NewLiteralExpr(scanner, value)
}

func (pc ParseContext) compileString(c ast.Children) rel.Expr {
	scanner := c.(ast.One).Node.One("").Scanner()
	return rel.NewLiteralExpr(scanner, rel.NewString([]rune(parseArraiString(scanner.String()))))
}

func (pc ParseContext) compileNumber(c ast.Children) rel.Expr {
	scanner := c.(ast.One).Node.One("").Scanner()
	n, err := strconv.ParseFloat(scanner.String(), 64)
	if err != nil {
		panic("Wat?")
	}
	return rel.NewLiteralExpr(scanner, rel.NewNumber(n))
}

func (pc ParseContext) compileChar(c ast.Children) rel.Expr {
	scanner := c.(ast.One).Node.One("").Scanner()
	char := scanner.String()[1:]
	runes := []rune(parseArraiStringFragment(char, "\"", ""))
	return rel.NewLiteralExpr(scanner, rel.NewNumber(float64(runes[0])))
}

func (pc ParseContext) compileExpr(b ast.Branch, c ast.Children) rel.Expr {
	switch c := c.(type) {
	case ast.One:
		expr := pc.CompileExpr(c.Node.(ast.Branch))
		if b.One("odelim") != nil {
			return rel.NewExprExpr(delimsScanner(b), expr)
		}
		return expr
	case ast.Many:
		if len(c) == 1 {
			return pc.CompileExpr(c[0].(ast.Branch))
		}
		panic("too many expr children")
	}
	return nil
}

func which(b ast.Branch, names ...string) (string, ast.Children) {
	if len(names) == 0 {
		panic("wat?")
	}
	for _, name := range names {
		if children, has := b[name]; has {
			return name, children
		}
	}
	return "", nil
}

func dotUnary(f binOpFunc) unOpFunc {
	return func(scanner parser.Scanner, e rel.Expr) rel.Expr {
		// TODO: Is scanner a suitable argument for rel.NewIdentExpr?
		return f(scanner, rel.NewIdentExpr(scanner, "."), e)
	}
}

type unOpFunc func(scanner parser.Scanner, e rel.Expr) rel.Expr

var unops = map[string]unOpFunc{
	"+":  rel.NewPosExpr,
	"-":  rel.NewNegExpr,
	"^":  rel.NewPowerSetExpr,
	"!":  rel.NewNotExpr,
	"*":  rel.NewEvalExpr,
	"//": NewPackageExpr,
	"=>": dotUnary(rel.NewDArrowExpr),
	">>": dotUnary(rel.NewSeqArrowExpr(false)),
	// TODO: >>>
	":>": dotUnary(rel.NewTupleMapExpr),
}

type binOpFunc func(scanner parser.Scanner, a, b rel.Expr) rel.Expr

var binops = map[string]binOpFunc{
	"->":      rel.NewArrowExpr,
	"=>":      rel.NewDArrowExpr,
	">>":      rel.NewSeqArrowExpr(false),
	">>>":     rel.NewSeqArrowExpr(true),
	":>":      rel.NewTupleMapExpr,
	"orderby": rel.NewOrderByExpr,
	"order":   rel.NewOrderExpr,
	"rank":    rel.NewRankExpr,
	"where":   rel.NewWhereExpr,
	"sum":     rel.NewSumExpr,
	"max":     rel.NewMaxExpr,
	"mean":    rel.NewMeanExpr,
	"median":  rel.NewMedianExpr,
	"min":     rel.NewMinExpr,
	"with":    rel.NewWithExpr,
	"without": rel.NewWithoutExpr,
	"&&":      rel.NewAndExpr,
	"||":      rel.NewOrExpr,
	"+":       rel.NewAddExpr,
	"-":       rel.NewSubExpr,
	"++":      rel.NewConcatExpr,
	"&~":      rel.NewDiffExpr,
	"~~":      rel.NewSymmDiffExpr,
	"&":       rel.NewIntersectExpr,
	"|":       rel.NewUnionExpr,
	"<&>":     rel.NewJoinExpr,
	"<->":     rel.NewComposeExpr,
	"-&-":     rel.NewJoinCommonExpr,
	"---":     rel.NewJoinExistsExpr,
	"-&>":     rel.NewRightMatchExpr,
	"<&-":     rel.NewLeftMatchExpr,
	"-->":     rel.NewRightResidueExpr,
	"<--":     rel.NewLeftResidueExpr,
	"*":       rel.NewMulExpr,
	"/":       rel.NewDivExpr,
	"%":       rel.NewModExpr,
	"-%":      rel.NewSubModExpr,
	"//":      rel.NewIdivExpr,
	"^":       rel.NewPowExpr,
	"\\":      rel.NewOffsetExpr,
	"+>":      rel.NewAddArrowExpr,
}

var compareOps = map[string]rel.CompareFunc{
	"<:": func(a, b rel.Value) (bool, error) {
		set, is := b.(rel.Set)
		if !is {
			return false, fmt.Errorf("<: rhs not a set: %v", b)
		}
		return set.Has(a), nil
	},
	"!<:": func(a, b rel.Value) (bool, error) {
		set, is := b.(rel.Set)
		if !is {
			return false, fmt.Errorf("!<: rhs not a set: %v", b)
		}
		return !set.Has(a), nil
	},
	"=":  func(a, b rel.Value) (bool, error) { return a.Equal(b), nil },
	"!=": func(a, b rel.Value) (bool, error) { return !a.Equal(b), nil },
	"<":  func(a, b rel.Value) (bool, error) { return a.Less(b), nil },
	">":  func(a, b rel.Value) (bool, error) { return b.Less(a), nil },
	"<=": func(a, b rel.Value) (bool, error) { return !b.Less(a), nil },
	">=": func(a, b rel.Value) (bool, error) { return !a.Less(b), nil },

	"(<)":   func(a, b rel.Value) (bool, error) { return subset(a, b), nil },
	"(>)":   func(a, b rel.Value) (bool, error) { return subset(b, a), nil },
	"(<=)":  func(a, b rel.Value) (bool, error) { return subsetOrEqual(a, b), nil },
	"(>=)":  func(a, b rel.Value) (bool, error) { return subsetOrEqual(b, a), nil },
	"(<>)":  func(a, b rel.Value) (bool, error) { return subsetOrSuperset(a, b), nil },
	"(<>=)": func(a, b rel.Value) (bool, error) { return subsetSupersetOrEqual(b, a), nil },

	"!(<)":   func(a, b rel.Value) (bool, error) { return !subset(a, b), nil },
	"!(>)":   func(a, b rel.Value) (bool, error) { return !subset(b, a), nil },
	"!(<=)":  func(a, b rel.Value) (bool, error) { return !subsetOrEqual(a, b), nil },
	"!(>=)":  func(a, b rel.Value) (bool, error) { return !subsetOrEqual(b, a), nil },
	"!(<>)":  func(a, b rel.Value) (bool, error) { return !subsetOrSuperset(a, b), nil },
	"!(<>=)": func(a, b rel.Value) (bool, error) { return !subsetSupersetOrEqual(b, a), nil },
}
