// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
)

// Error instances.
var (
	errInvalidOperation        = terror.ClassExpression.New(codeInvalidOperation, "invalid operation")
	errIncorrectParameterCount = terror.ClassExpression.New(codeIncorrectParameterCount, "Incorrect parameter count in the call to native function '%s'")
	errFunctionNotExists       = terror.ClassExpression.New(codeFunctionNotExists, "FUNCTION %s does not exist")
)

// Error codes.
const (
	codeInvalidOperation        terror.ErrCode = 1
	codeIncorrectParameterCount                = 1582
	codeFunctionNotExists                      = 1305
)

// EvalAstExpr evaluates ast expression directly.
var EvalAstExpr func(expr ast.ExprNode, ctx context.Context) (types.Datum, error)

// baseExpr will be removed later.
// baseExpr implements the common implementation of EvalXXX(XXX:Int/Real/Decimal/String) of Expression to guarantee the
// availability of Constant and Column.
type baseExpr struct {
	self Expression
}

func (be *baseExpr) SetSelf(expr Expression) {
	be.self = expr
}

func (be *baseExpr) EvalInt(row []types.Datum, sc *variable.StatementContext) (int64, bool, error) {
	if be.self.GetType().Tp == mysql.TypeNull {
		return 0, true, nil
	}
	val, err := be.self.Eval(row)
	if err != nil || val.IsNull() {
		return 0, val.IsNull(), errors.Trace(err)
	}
	switch be.self.GetType().ToClass() {
	case types.ClassInt:
		return val.GetInt64(), false, nil
	default:
		res, err := val.ToInt64(sc)
		return res, false, errors.Trace(err)
	}
}

func (be *baseExpr) EvalReal(row []types.Datum, sc *variable.StatementContext) (float64, bool, error) {
	if be.self.GetType().Tp == mysql.TypeNull {
		return 0, true, nil
	}
	val, err := be.self.Eval(row)
	if err != nil || val.IsNull() {
		return 0, val.IsNull(), errors.Trace(err)
	}
	switch be.self.GetType().ToClass() {
	case types.ClassReal:
		return val.GetFloat64(), false, nil
	default:
		res, err := val.ToFloat64(sc)
		return res, false, errors.Trace(err)
	}
}

func (be *baseExpr) EvalString(row []types.Datum, sc *variable.StatementContext) (string, bool, error) {
	if be.self.GetType().Tp == mysql.TypeNull {
		return "", true, nil
	}
	val, err := be.self.Eval(row)
	if err != nil || val.IsNull() {
		return "", val.IsNull(), errors.Trace(err)
	}
	// We cannot use val.GetString() even if b.self.GetType().ToClass() == types.ClassString here,
	// because the types like types.KindMysqlHex will get an empty value.
	res, err := val.ToString()
	return res, false, errors.Trace(err)
}

func (be *baseExpr) EvalDecimal(row []types.Datum, sc *variable.StatementContext) (*types.MyDecimal, bool, error) {
	if be.self.GetType().Tp == mysql.TypeNull {
		return nil, true, nil
	}
	val, err := be.self.Eval(row)
	if err != nil || val.IsNull() {
		return nil, val.IsNull(), errors.Trace(err)
	}
	switch be.self.GetType().ToClass() {
	case types.ClassDecimal:
		return val.GetMysqlDecimal(), false, nil
	default:
		res, err := val.ToDecimal(sc)
		return res, false, errors.Trace(err)
	}
}

// Expression represents all scalar expression in SQL.
type Expression interface {
	fmt.Stringer
	json.Marshaler

	// Eval evaluates an expression through a row.
	Eval(row []types.Datum) (types.Datum, error)

	// EvalInt returns the int64 representation of expression.
	EvalInt(row []types.Datum, sc *variable.StatementContext) (val int64, isNull bool, err error)

	// EvalReal returns the float64 representation of expression.
	EvalReal(row []types.Datum, sc *variable.StatementContext) (val float64, isNull bool, err error)

	// EvalString returns the string representation of expression.
	EvalString(row []types.Datum, sc *variable.StatementContext) (val string, isNull bool, err error)

	// EvalDecimal returns the decimal representation of expression.
	EvalDecimal(row []types.Datum, sc *variable.StatementContext) (val *types.MyDecimal, isNull bool, err error)

	// GetType gets the type that the expression returns.
	GetType() *types.FieldType

	// Clone copies an expression totally.
	Clone() Expression

	// HashCode create the hashcode for expression
	HashCode() []byte

	// Equal checks whether two expressions are equal.
	Equal(e Expression, ctx context.Context) bool

	// IsCorrelated checks if this expression has correlated key.
	IsCorrelated() bool

	// Decorrelate try to decorrelate the expression by schema.
	Decorrelate(schema *Schema) Expression

	// ResolveIndices resolves indices by the given schema.
	ResolveIndices(schema *Schema)
}

// CNFExprs stands for a CNF expression.
type CNFExprs []Expression

// Clone clones itself.
func (e CNFExprs) Clone() CNFExprs {
	cnf := make(CNFExprs, 0, len(e))
	for _, expr := range e {
		cnf = append(cnf, expr.Clone())
	}
	return cnf
}

// EvalBool evaluates expression list to a boolean value.
func EvalBool(exprList CNFExprs, row []types.Datum, ctx context.Context) (bool, error) {
	for _, expr := range exprList {
		data, err := expr.Eval(row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if data.IsNull() {
			return false, nil
		}

		i, err := data.ToBool(ctx.GetSessionVars().StmtCtx)
		if err != nil {
			return false, errors.Trace(err)
		}
		if i == 0 {
			return false, nil
		}
	}
	return true, nil
}

// One stands for a number 1.
var One = &Constant{
	Value:   types.NewDatum(1),
	RetType: types.NewFieldType(mysql.TypeTiny),
}

// Zero stands for a number 0.
var Zero = &Constant{
	Value:   types.NewDatum(0),
	RetType: types.NewFieldType(mysql.TypeTiny),
}

// Null stands for null constant.
var Null = &Constant{
	Value:   types.NewDatum(nil),
	RetType: types.NewFieldType(mysql.TypeTiny),
}

// Constant stands for a constant value.
type Constant struct {
	baseExpr
	Value   types.Datum
	RetType *types.FieldType
}

// String implements fmt.Stringer interface.
func (c *Constant) String() string {
	return fmt.Sprintf("%v", c.Value.GetValue())
}

// MarshalJSON implements json.Marshaler interface.
func (c *Constant) MarshalJSON() ([]byte, error) {
	buffer := bytes.NewBufferString(fmt.Sprintf("\"%s\"", c))
	return buffer.Bytes(), nil
}

// Clone implements Expression interface.
func (c *Constant) Clone() Expression {
	con := *c
	return &con
}

// GetType implements Expression interface.
func (c *Constant) GetType() *types.FieldType {
	return c.RetType
}

// Eval implements Expression interface.
func (c *Constant) Eval(_ []types.Datum) (types.Datum, error) {
	return c.Value, nil
}

// Equal implements Expression interface.
func (c *Constant) Equal(b Expression, ctx context.Context) bool {
	y, ok := b.(*Constant)
	if !ok {
		return false
	}
	con, err := c.Value.CompareDatum(ctx.GetSessionVars().StmtCtx, y.Value)
	if err != nil || con != 0 {
		return false
	}
	return true
}

// IsCorrelated implements Expression interface.
func (c *Constant) IsCorrelated() bool {
	return false
}

// Decorrelate implements Expression interface.
func (c *Constant) Decorrelate(_ *Schema) Expression {
	return c
}

// HashCode implements Expression interface.
func (c *Constant) HashCode() []byte {
	var bytes []byte
	bytes, _ = codec.EncodeValue(bytes, c.Value)
	return bytes
}

// ResolveIndices implements Expression interface.
func (c *Constant) ResolveIndices(_ *Schema) {
}

// composeConditionWithBinaryOp composes condition with binary operator into a balance deep tree, which benefits a lot for pb decoder/encoder.
func composeConditionWithBinaryOp(ctx context.Context, conditions []Expression, funcName string) Expression {
	length := len(conditions)
	if length == 0 {
		return nil
	}
	if length == 1 {
		return conditions[0]
	}
	expr, _ := NewFunction(ctx, funcName,
		types.NewFieldType(mysql.TypeTiny),
		composeConditionWithBinaryOp(ctx, conditions[:length/2], funcName),
		composeConditionWithBinaryOp(ctx, conditions[length/2:], funcName))
	return expr
}

// ComposeCNFCondition composes CNF items into a balance deep CNF tree, which benefits a lot for pb decoder/encoder.
func ComposeCNFCondition(ctx context.Context, conditions ...Expression) Expression {
	return composeConditionWithBinaryOp(ctx, conditions, ast.AndAnd)
}

// ComposeDNFCondition composes DNF items into a balance deep DNF tree.
func ComposeDNFCondition(ctx context.Context, conditions ...Expression) Expression {
	return composeConditionWithBinaryOp(ctx, conditions, ast.OrOr)
}

// Assignment represents a set assignment in Update, such as
// Update t set c1 = hex(12), c2 = c3 where c2 = 1
type Assignment struct {
	Col  *Column
	Expr Expression
}

// VarAssignment represents a variable assignment in Set, such as set global a = 1.
type VarAssignment struct {
	Name        string
	Expr        Expression
	IsDefault   bool
	IsGlobal    bool
	IsSystem    bool
	ExtendValue *Constant
}

// splitNormalFormItems split CNF(conjunctive normal form) like "a and b and c", or DNF(disjunctive normal form) like "a or b or c"
func splitNormalFormItems(onExpr Expression, funcName string) []Expression {
	switch v := onExpr.(type) {
	case *ScalarFunction:
		if v.FuncName.L == funcName {
			var ret []Expression
			for _, arg := range v.GetArgs() {
				ret = append(ret, splitNormalFormItems(arg, funcName)...)
			}
			return ret
		}
	}
	return []Expression{onExpr}
}

// SplitCNFItems splits CNF items.
// CNF means conjunctive normal form, e.g. "a and b and c".
func SplitCNFItems(onExpr Expression) []Expression {
	return splitNormalFormItems(onExpr, ast.AndAnd)
}

// SplitDNFItems splits DNF items.
// DNF means disjunctive normal form, e.g. "a or b or c".
func SplitDNFItems(onExpr Expression) []Expression {
	return splitNormalFormItems(onExpr, ast.OrOr)
}

// EvaluateExprWithNull sets columns in schema as null and calculate the final result of the scalar function.
// If the Expression is a non-constant value, it means the result is unknown.
func EvaluateExprWithNull(ctx context.Context, schema *Schema, expr Expression) (Expression, error) {
	switch x := expr.(type) {
	case *ScalarFunction:
		var err error
		args := make([]Expression, len(x.GetArgs()))
		for i, arg := range x.GetArgs() {
			args[i], err = EvaluateExprWithNull(ctx, schema, arg)
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
		newFunc, err := NewFunction(ctx, x.FuncName.L, types.NewFieldType(mysql.TypeTiny), args...)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return FoldConstant(newFunc), nil
	case *Column:
		if !schema.Contains(x) {
			return x, nil
		}
		constant := &Constant{Value: types.Datum{}, RetType: types.NewFieldType(mysql.TypeNull)}
		return constant, nil
	default:
		return x.Clone(), nil
	}
}

// TableInfo2Schema converts table info to schema.
func TableInfo2Schema(tbl *model.TableInfo) *Schema {
	cols := ColumnInfos2Columns(tbl.Name, tbl.Columns)
	keys := make([]KeyInfo, 0, len(tbl.Indices)+1)
	for _, idx := range tbl.Indices {
		if !idx.Unique || idx.State != model.StatePublic {
			continue
		}
		ok := true
		newKey := make([]*Column, 0, len(idx.Columns))
		for _, idxCol := range idx.Columns {
			find := false
			for i, col := range tbl.Columns {
				if idxCol.Name.L == col.Name.L {
					if !mysql.HasNotNullFlag(col.Flag) {
						break
					}
					newKey = append(newKey, cols[i])
					find = true
					break
				}
			}
			if !find {
				ok = false
				break
			}
		}
		if ok {
			keys = append(keys, newKey)
		}
	}
	if tbl.PKIsHandle {
		for i, col := range tbl.Columns {
			if mysql.HasPriKeyFlag(col.Flag) {
				keys = append(keys, KeyInfo{cols[i]})
				break
			}
		}
	}
	schema := NewSchema(cols...)
	schema.SetUniqueKeys(keys)
	return schema
}

// ColumnInfos2Columns converts a slice of ColumnInfo to a slice of Column.
func ColumnInfos2Columns(tblName model.CIStr, colInfos []*model.ColumnInfo) []*Column {
	columns := make([]*Column, 0, len(colInfos))
	for i, col := range colInfos {
		newCol := &Column{
			ColName:  col.Name,
			TblName:  tblName,
			RetType:  &col.FieldType,
			Position: i,
		}
		columns = append(columns, newCol)
	}
	return columns
}

// NewCastFunc creates a new cast function.
func NewCastFunc(tp *types.FieldType, arg Expression, ctx context.Context) *ScalarFunction {
	bt := &builtinCastSig{newBaseBuiltinFunc([]Expression{arg}, ctx), tp}
	return &ScalarFunction{
		FuncName: model.NewCIStr(ast.Cast),
		RetType:  tp,
		Function: bt,
	}
}

// NewValuesFunc creates a new values function.
func NewValuesFunc(offset int, retTp *types.FieldType, ctx context.Context) *ScalarFunction {
	fc := &valuesFunctionClass{baseFunctionClass{ast.Values, 0, 0}, offset}
	bt, _ := fc.getFunction(nil, ctx)
	return &ScalarFunction{
		FuncName: model.NewCIStr(ast.Values),
		RetType:  retTp,
		Function: bt,
	}
}

func init() {
	expressionMySQLErrCodes := map[terror.ErrCode]uint16{
		codeIncorrectParameterCount: mysql.ErrWrongParamcountToNativeFct,
		codeFunctionNotExists:       mysql.ErrSpDoesNotExist,
	}
	terror.ErrClassToMySQLCodes[terror.ClassExpression] = expressionMySQLErrCodes
}
