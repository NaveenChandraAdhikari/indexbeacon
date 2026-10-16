package parser

import (
	"fmt"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Parse turns one SQL statement into a Shape. Statements idxa cannot use,
// such as DDL or a SELECT with no table, produce a Shape whose Unparsed field
// explains why, rather than an error, so a caller processing a whole workload
// can keep going and report coverage honestly.
func Parse(sql string) (*Shape, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(tree.Stmts) == 0 {
		return &Shape{SQL: sql, Unparsed: "empty statement"}, nil
	}

	shape := &Shape{SQL: sql}
	w := &walker{shape: shape, aliases: map[string]string{}}

	switch node := tree.Stmts[0].Stmt.Node.(type) {
	case *pg_query.Node_SelectStmt:
		shape.Command = "select"
		w.walkSelect(node.SelectStmt)
	case *pg_query.Node_UpdateStmt:
		shape.Command = "update"
		w.walkUpdate(node.UpdateStmt)
	case *pg_query.Node_DeleteStmt:
		shape.Command = "delete"
		w.walkDelete(node.DeleteStmt)
	case *pg_query.Node_InsertStmt:
		// An INSERT has no predicates worth indexing. It still matters for
		// the write cost of any index we might recommend, which the scorer
		// takes from table statistics rather than from the statement.
		shape.Command = "insert"
		shape.Unparsed = "insert statements do not produce index candidates"
	default:
		shape.Command = "other"
		shape.Unparsed = "statement kind cannot benefit from an index"
	}

	w.finish()
	return shape, nil
}

// Normalize replaces literals in a statement with parameter placeholders,
// matching what pg_stat_statements stores. Used to group sampled statements.
func Normalize(sql string) (string, error) {
	out, err := pg_query.Normalize(sql)
	if err != nil {
		return "", fmt.Errorf("normalize: %w", err)
	}
	return out, nil
}

// Fingerprint returns a stable hash of a statement's structure, ignoring
// literals. It is how sampled statements are grouped when pg_stat_statements
// is unavailable, and how statements are matched across nodes whose query
// identifiers disagree.
func Fingerprint(sql string) (string, error) {
	out, err := pg_query.Fingerprint(sql)
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	return out, nil
}

// walker accumulates state while descending one query scope.
//
// Scopes matter: a subquery or CTE has its own FROM clause, so an unqualified
// column inside it must be resolved against that scope's tables, not against
// every table mentioned anywhere in the statement. Walking flat would either
// drop those predicates or attribute them to the wrong relation, and a
// predicate on the wrong table produces an index recommendation on the wrong
// table.
type walker struct {
	shape *Shape
	// aliases maps an alias, or a bare table name, to the qualified table,
	// for this scope only.
	aliases map[string]string
	// tables are the tables introduced by this scope's FROM clause.
	tables []TableRef
	// pending holds predicates found in this scope, awaiting resolution
	// once the scope's full FROM clause is known.
	pending      []Predicate
	pendingOrder []OrderCol
	pendingGroup []OrderCol
	// inOr tracks whether the current boolean context is a disjunction.
	inOr bool
}

// child returns a walker for a nested query scope, sharing the output shape
// but resolving names independently.
func (w *walker) child() *walker {
	return &walker{shape: w.shape, aliases: map[string]string{}}
}

func (w *walker) walkSelect(s *pg_query.SelectStmt) {
	if s == nil {
		return
	}

	// A set operation (UNION and friends) has no FROM of its own; walk both
	// arms so their tables and predicates are still collected.
	if s.Op != pg_query.SetOperation_SETOP_NONE {
		// Each arm of a set operation is its own scope.
		l, r := w.child(), w.child()
		l.walkSelect(s.Larg)
		l.finish()
		r.walkSelect(s.Rarg)
		r.finish()
		return
	}

	// WITH clauses are walked so predicates inside CTEs are not lost.
	if s.WithClause != nil {
		for _, cte := range s.WithClause.Ctes {
			if c, ok := cte.Node.(*pg_query.Node_CommonTableExpr); ok {
				if sub, ok := c.CommonTableExpr.Ctequery.Node.(*pg_query.Node_SelectStmt); ok {
					cw := w.child()
					cw.walkSelect(sub.SelectStmt)
					cw.finish()
				}
			}
		}
	}

	for _, from := range s.FromClause {
		w.walkFrom(from)
	}
	w.walkQual(s.WhereClause)

	for _, g := range s.GroupClause {
		if col, ok := w.columnOf(g); ok {
			w.pendingGroup = append(w.pendingGroup, OrderCol{Table: w.resolveAlias(col.table), Column: col.name})
		}
	}
	w.walkSortClause(s.SortClause)
	if s.LimitCount != nil {
		w.shape.HasLimit = true
	}
}

func (w *walker) walkUpdate(u *pg_query.UpdateStmt) {
	if u == nil {
		return
	}
	w.addRangeVar(u.Relation)
	for _, from := range u.FromClause {
		w.walkFrom(from)
	}
	w.walkQual(u.WhereClause)
}

func (w *walker) walkDelete(d *pg_query.DeleteStmt) {
	if d == nil {
		return
	}
	w.addRangeVar(d.Relation)
	for _, from := range d.UsingClause {
		w.walkFrom(from)
	}
	w.walkQual(d.WhereClause)
}

// walkSortClause records ORDER BY entries that refer to plain columns.
func (w *walker) walkSortClause(sorts []*pg_query.Node) {
	for _, node := range sorts {
		sb, ok := node.Node.(*pg_query.Node_SortBy)
		if !ok {
			continue
		}
		col, ok := w.columnOf(sb.SortBy.Node)
		if !ok {
			continue
		}
		oc := OrderCol{
			Table:  w.resolveAlias(col.table),
			Column: col.name,
			Desc:   sb.SortBy.SortbyDir == pg_query.SortByDir_SORTBY_DESC,
		}
		switch sb.SortBy.SortbyNulls {
		case pg_query.SortByNulls_SORTBY_NULLS_FIRST:
			v := true
			oc.NullsFirst = &v
		case pg_query.SortByNulls_SORTBY_NULLS_LAST:
			v := false
			oc.NullsFirst = &v
		}
		w.pendingOrder = append(w.pendingOrder, oc)
	}
}

// walkFrom descends a FROM item, which is either a table, a join, or a
// subquery.
func (w *walker) walkFrom(node *pg_query.Node) {
	if node == nil {
		return
	}
	switch n := node.Node.(type) {
	case *pg_query.Node_RangeVar:
		w.addRangeVar(n.RangeVar)
	case *pg_query.Node_JoinExpr:
		w.walkFrom(n.JoinExpr.Larg)
		w.walkFrom(n.JoinExpr.Rarg)
		// Join conditions are predicates too: an equi-join wants an index on
		// the column of whichever side is scanned second.
		w.walkQualWith(n.JoinExpr.Quals, true)
	case *pg_query.Node_RangeSubselect:
		if sub, ok := n.RangeSubselect.Subquery.Node.(*pg_query.Node_SelectStmt); ok {
			sw := w.child()
			sw.walkSelect(sub.SelectStmt)
			sw.finish()
		}
	}
}

// addRangeVar records a table and its alias.
func (w *walker) addRangeVar(rv *pg_query.RangeVar) {
	if rv == nil || rv.Relname == "" {
		return
	}
	t := TableRef{Schema: rv.Schemaname, Name: rv.Relname, Alias: rv.Relname}
	if rv.Alias != nil && rv.Alias.Aliasname != "" {
		t.Alias = rv.Alias.Aliasname
	}
	w.tables = append(w.tables, t)
	w.shape.Tables = append(w.shape.Tables, t)
	w.aliases[t.Alias] = t.Qualified()
	// The bare table name also resolves, so `orders.id` works even when the
	// query gave the table an alias.
	w.aliases[t.Name] = t.Qualified()
}

// finish resolves this scope's predicates against this scope's tables and
// appends them to the shared shape. An unqualified column is attributed to
// the scope's only table; when a scope has several tables an unqualified
// column is ambiguous, so it is dropped rather than guessed at.
func (w *walker) finish() {
	var single string
	if len(w.tables) == 1 {
		single = w.tables[0].Qualified()
	}

	for _, p := range w.pending {
		if p.Table == "" {
			p.Table = single
		}
		if p.Table == "" {
			continue // ambiguous, and a wrong table is worse than none
		}
		w.shape.Predicates = append(w.shape.Predicates, p)
	}
	for _, o := range w.pendingOrder {
		if o.Table == "" {
			o.Table = single
		}
		if o.Table == "" {
			continue
		}
		w.shape.OrderBy = append(w.shape.OrderBy, o)
	}
	for _, g := range w.pendingGroup {
		if g.Table == "" {
			g.Table = single
		}
		if g.Table == "" {
			continue
		}
		w.shape.GroupBy = append(w.shape.GroupBy, g)
	}

	w.pending, w.pendingOrder, w.pendingGroup = nil, nil, nil
}

// walkQual descends a boolean expression collecting predicates.
func (w *walker) walkQual(node *pg_query.Node) { w.walkQualWith(node, false) }

func (w *walker) walkQualWith(node *pg_query.Node, fromJoin bool) {
	if node == nil {
		return
	}
	switch n := node.Node.(type) {
	case *pg_query.Node_BoolExpr:
		be := n.BoolExpr
		switch be.Boolop {
		case pg_query.BoolExprType_AND_EXPR:
			for _, arg := range be.Args {
				w.walkQualWith(arg, fromJoin)
			}
		case pg_query.BoolExprType_OR_EXPR:
			// Predicates under an OR are recorded but flagged, because a
			// disjunction does not restrict every branch simultaneously and
			// so must not be merged into one composite index.
			prev := w.inOr
			w.inOr = true
			for _, arg := range be.Args {
				w.walkQualWith(arg, fromJoin)
			}
			w.inOr = prev
		case pg_query.BoolExprType_NOT_EXPR:
			// A negation cannot drive an index scan; skip its contents.
		}
	case *pg_query.Node_AExpr:
		w.walkAExpr(n.AExpr, fromJoin)
	case *pg_query.Node_NullTest:
		nt := n.NullTest
		col, ok := w.columnOf(nt.Arg)
		if !ok {
			return
		}
		op := OpIsNull
		if nt.Nulltesttype == pg_query.NullTestType_IS_NOT_NULL {
			op = OpIsNotNull
		}
		w.add(Predicate{Table: col.table, Column: col.name, Expr: col.expr, Op: op, FromJoin: fromJoin})
	case *pg_query.Node_ScalarArrayOpExpr:
		// `col = ANY($1)`, the parsed form of IN with a parameter.
		w.add(Predicate{Op: OpIn, FromJoin: fromJoin})
	case *pg_query.Node_SubLink:
		// Walk the subquery so its own predicates are collected.
		if sub, ok := n.SubLink.Subselect.Node.(*pg_query.Node_SelectStmt); ok {
			sw := w.child()
			sw.walkSelect(sub.SelectStmt)
			sw.finish()
		}
		if col, ok := w.columnOf(n.SubLink.Testexpr); ok {
			w.add(Predicate{Table: col.table, Column: col.name, Expr: col.expr, Op: OpIn, FromJoin: fromJoin})
		}
	}
}

// walkAExpr handles the ordinary binary comparisons.
func (w *walker) walkAExpr(e *pg_query.A_Expr, fromJoin bool) {
	if e == nil {
		return
	}
	opName := operatorName(e.Name)

	// A join condition compares two columns; record both sides, because
	// either could be the one needing an index.
	lcol, lok := w.columnOf(e.Lexpr)
	rcol, rok := w.columnOf(e.Rexpr)
	if lok && rok && e.Kind == pg_query.A_Expr_Kind_AEXPR_OP && opName == "=" {
		w.add(Predicate{Table: lcol.table, Column: lcol.name, Expr: lcol.expr, Op: OpEq, FromJoin: true})
		w.add(Predicate{Table: rcol.table, Column: rcol.name, Expr: rcol.expr, Op: OpEq, FromJoin: true})
		return
	}

	// Otherwise one side is the column and the other is a value.
	col, ok := lcol, lok
	value := e.Rexpr
	if !ok {
		col, ok = rcol, rok
		value = e.Lexpr
	}
	if !ok {
		return
	}

	p := Predicate{Table: col.table, Column: col.name, Expr: col.expr, FromJoin: fromJoin, FromOr: w.inOr}
	lit, isParam := literalOf(value)
	p.Const, p.Param = lit, isParam

	switch e.Kind {
	case pg_query.A_Expr_Kind_AEXPR_OP:
		switch opName {
		case "=":
			p.Op = OpEq
		case "<", "<=", ">", ">=":
			p.Op = OpRange
		case "<>", "!=":
			// Inequality is not selective enough to index for.
			p.Op = OpOther
		case "@>", "<@", "&&", "?", "?&", "?|":
			// Containment and overlap, on jsonb or arrays. No btree can
			// answer these, so they call for an inverted index.
			p.Op = OpContains
		case "@@":
			p.Op = OpTextSearch
		default:
			p.Op = OpOther
		}
	case pg_query.A_Expr_Kind_AEXPR_BETWEEN, pg_query.A_Expr_Kind_AEXPR_NOT_BETWEEN:
		p.Op = OpRange
	case pg_query.A_Expr_Kind_AEXPR_IN:
		p.Op = OpIn
	case pg_query.A_Expr_Kind_AEXPR_LIKE:
		// Only a prefix pattern can use a btree index, and only under the C
		// collation or a text_pattern_ops operator class.
		switch {
		case lit != "" && !isParam && isPrefixPattern(lit):
			p.Op = OpLikePrefix
			p.Const = strings.TrimSuffix(lit, "%")
		case lit != "" && !isParam:
			// An unanchored pattern. A btree cannot help, but a trigram
			// index can, so it is worth recording rather than discarding.
			p.Op = OpLikeContains
		default:
			// A parameterised LIKE could be either shape; assume the harder
			// one, since suggesting a btree that will not be used is worse
			// than suggesting nothing.
			p.Op = OpLikeContains
		}
	default:
		p.Op = OpOther
	}
	w.add(p)
}

// resolveAlias maps an alias written in the query to the qualified table.
func (w *walker) resolveAlias(name string) string {
	if name == "" {
		return ""
	}
	if q, ok := w.aliases[name]; ok {
		return q
	}
	return name
}

// add records a predicate, resolving its table through the alias map.
func (w *walker) add(p Predicate) {
	if p.Column == "" && p.Expr == "" {
		return
	}
	if p.Table != "" {
		if q, ok := w.aliases[p.Table]; ok {
			p.Table = q
		}
	}
	if w.inOr {
		p.FromOr = true
	}
	w.pending = append(w.pending, p)
}

// colRef is a column reference resolved out of the parse tree.
type colRef struct {
	table string // alias or table name as written, resolved later
	name  string
	expr  string // set instead of name when the reference is an expression
}

// columnOf extracts a column reference from an expression node, recognising
// both bare columns and casts of columns, and rendering function calls over a
// column as expressions.
func (w *walker) columnOf(node *pg_query.Node) (colRef, bool) {
	if node == nil {
		return colRef{}, false
	}
	switch n := node.Node.(type) {
	case *pg_query.Node_ColumnRef:
		parts := make([]string, 0, len(n.ColumnRef.Fields))
		for _, f := range n.ColumnRef.Fields {
			s, ok := f.Node.(*pg_query.Node_String_)
			if !ok {
				// A star, as in count(*): not a column reference.
				return colRef{}, false
			}
			parts = append(parts, s.String_.Sval)
		}
		switch len(parts) {
		case 1:
			return colRef{name: parts[0]}, true
		case 2:
			return colRef{table: parts[0], name: parts[1]}, true
		case 3:
			// schema.table.column
			return colRef{table: parts[0] + "." + parts[1], name: parts[2]}, true
		}
		return colRef{}, false
	case *pg_query.Node_TypeCast:
		// `col::text = ...` still constrains col.
		return w.columnOf(n.TypeCast.Arg)
	case *pg_query.Node_FuncCall:
		// A function over a column needs an expression index. Render it so
		// the candidate generator can report it, using the first column
		// argument to attribute the table.
		var inner colRef
		found := false
		for _, arg := range n.FuncCall.Args {
			if c, ok := w.columnOf(arg); ok {
				inner = c
				found = true
				break
			}
		}
		if !found {
			return colRef{}, false
		}
		name := funcName(n.FuncCall.Funcname)
		if name == "" {
			return colRef{}, false
		}
		col := inner.name
		if col == "" {
			col = inner.expr
		}
		return colRef{table: inner.table, expr: name + "(" + col + ")"}, true
	}
	return colRef{}, false
}

// literalOf renders the constant on the value side of a comparison, and
// reports whether it was a parameter placeholder instead.
func literalOf(node *pg_query.Node) (lit string, isParam bool) {
	if node == nil {
		return "", false
	}
	switch n := node.Node.(type) {
	case *pg_query.Node_ParamRef:
		return "", true
	case *pg_query.Node_TypeCast:
		return literalOf(n.TypeCast.Arg)
	case *pg_query.Node_AConst:
		if n.AConst.Isnull {
			return "", false
		}
		switch v := n.AConst.Val.(type) {
		case *pg_query.A_Const_Sval:
			return v.Sval.Sval, false
		case *pg_query.A_Const_Ival:
			return strconv.FormatInt(int64(v.Ival.Ival), 10), false
		case *pg_query.A_Const_Fval:
			return v.Fval.Fval, false
		case *pg_query.A_Const_Boolval:
			return strconv.FormatBool(v.Boolval.Boolval), false
		}
	}
	return "", false
}

// operatorName renders an operator name from its node list.
func operatorName(nodes []*pg_query.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	last := nodes[len(nodes)-1]
	if s, ok := last.Node.(*pg_query.Node_String_); ok {
		return s.String_.Sval
	}
	return ""
}

// funcName renders a function name, lowercased, ignoring the schema.
func funcName(nodes []*pg_query.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	last := nodes[len(nodes)-1]
	if s, ok := last.Node.(*pg_query.Node_String_); ok {
		return strings.ToLower(s.String_.Sval)
	}
	return ""
}

// isPrefixPattern reports whether a LIKE pattern is anchored at the start,
// which is the only form a btree index can accelerate.
func isPrefixPattern(pattern string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "%") || strings.HasPrefix(pattern, "_") {
		return false
	}
	// The wildcard must appear only at the end.
	idx := strings.IndexAny(pattern, "%_")
	return idx == len(pattern)-1 && strings.HasSuffix(pattern, "%")
}
