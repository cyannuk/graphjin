package psql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/dosco/super-graph/qcode"
	"github.com/dosco/super-graph/util"
)

const (
	empty      = ""
	closeBlock = 500
)

type Variables map[string]json.RawMessage

type Config struct {
	Schema *DBSchema
	Vars   map[string]string
}

type Compiler struct {
	schema *DBSchema
	vars   map[string]string
}

func NewCompiler(conf Config) *Compiler {
	return &Compiler{conf.Schema, conf.Vars}
}

func (c *Compiler) AddRelationship(child, parent string, rel *DBRel) error {
	return c.schema.SetRel(child, parent, rel)
}

func (c *Compiler) IDColumn(table string) (string, error) {
	t, err := c.schema.GetTable(table)
	if err != nil {
		return empty, err
	}

	return t.PrimaryCol, nil
}

type compilerContext struct {
	w io.Writer
	s []qcode.Select
	*Compiler
}

func (co *Compiler) CompileEx(qc *qcode.QCode, vars Variables) (uint32, []byte, error) {
	w := &bytes.Buffer{}
	skipped, err := co.Compile(qc, w, vars)
	return skipped, w.Bytes(), err
}

func (co *Compiler) Compile(qc *qcode.QCode, w io.Writer, vars Variables) (uint32, error) {
	switch qc.Type {
	case qcode.QTQuery:
		return co.compileQuery(qc, w)
	case qcode.QTInsert, qcode.QTUpdate, qcode.QTDelete, qcode.QTUpsert:
		return co.compileMutation(qc, w, vars)
	}

	return 0, fmt.Errorf("Unknown operation type %d", qc.Type)
}

func (co *Compiler) compileQuery(qc *qcode.QCode, w io.Writer) (uint32, error) {
	if len(qc.Selects) == 0 {
		return 0, errors.New("empty query")
	}

	c := &compilerContext{w, qc.Selects, co}
	root := &qc.Selects[0]

	ti, err := c.schema.GetTable(root.Table)
	if err != nil {
		return 0, err
	}

	st := NewStack()
	st.Push(root.ID + closeBlock)
	st.Push(root.ID)

	//fmt.Fprintf(w, `SELECT json_object_agg('%s', %s) FROM (`,
	//root.FieldName, root.Table)
	io.WriteString(c.w, `SELECT json_object_agg('`)
	io.WriteString(c.w, root.FieldName)
	io.WriteString(c.w, `', `)

	if ti.Singular == false {
		io.WriteString(c.w, root.Table)
	} else {
		io.WriteString(c.w, "sel_json_")
		int2string(c.w, root.ID)
	}
	io.WriteString(c.w, `) FROM (`)

	var ignored uint32

	for {
		if st.Len() == 0 {
			break
		}

		id := st.Pop()

		if id < closeBlock {
			sel := &c.s[id]

			ti, err := c.schema.GetTable(sel.Table)
			if err != nil {
				return 0, err
			}

			if sel.ID != 0 {
				if err = c.renderJoin(sel); err != nil {
					return 0, err
				}
			}
			skipped, err := c.renderSelect(sel, ti)
			if err != nil {
				return 0, err
			}
			ignored |= skipped

			for _, cid := range sel.Children {
				if hasBit(skipped, uint32(cid)) {
					continue
				}
				child := &c.s[cid]

				st.Push(child.ID + closeBlock)
				st.Push(child.ID)
			}

		} else {
			sel := &c.s[(id - closeBlock)]

			ti, err := c.schema.GetTable(sel.Table)
			if err != nil {
				return 0, err
			}

			err = c.renderSelectClose(sel, ti)
			if err != nil {
				return 0, err
			}

			if sel.ID != 0 {
				if err = c.renderJoinClose(sel); err != nil {
					return 0, err
				}
			}
		}
	}

	io.WriteString(c.w, `)`)
	alias(c.w, `done_1337`)

	return ignored, nil
}

func (c *compilerContext) processChildren(sel *qcode.Select, ti *DBTableInfo) (uint32, []*qcode.Column) {
	var skipped uint32

	cols := make([]*qcode.Column, 0, len(sel.Cols))
	colmap := make(map[string]struct{}, len(sel.Cols))

	for i := range sel.Cols {
		colmap[sel.Cols[i].Name] = struct{}{}
	}

	for _, id := range sel.Children {
		child := &c.s[id]

		rel, err := c.schema.GetRel(child.Table, ti.Name)
		if err != nil {
			skipped |= (1 << uint(id))
			continue
		}

		switch rel.Type {
		case RelOneToMany:
			fallthrough
		case RelBelongTo:
			if _, ok := colmap[rel.Col2]; !ok {
				cols = append(cols, &qcode.Column{ti.Name, rel.Col2, rel.Col2})
			}
		case RelOneToManyThrough:
			if _, ok := colmap[rel.Col1]; !ok {
				cols = append(cols, &qcode.Column{ti.Name, rel.Col1, rel.Col1})
			}
		case RelRemote:
			if _, ok := colmap[rel.Col1]; !ok {
				cols = append(cols, &qcode.Column{ti.Name, rel.Col1, rel.Col2})
			}
			skipped |= (1 << uint(id))

		default:
			skipped |= (1 << uint(id))
		}
	}

	return skipped, cols
}

func (c *compilerContext) renderSelect(sel *qcode.Select, ti *DBTableInfo) (uint32, error) {
	skipped, childCols := c.processChildren(sel, ti)
	hasOrder := len(sel.OrderBy) != 0

	// SELECT
	if ti.Singular == false {
		//fmt.Fprintf(w, `SELECT coalesce(json_agg("%s"`, c.sel.Table)
		io.WriteString(c.w, `SELECT coalesce(json_agg("`)
		io.WriteString(c.w, "sel_json_")
		int2string(c.w, sel.ID)
		io.WriteString(c.w, `"`)

		if hasOrder {
			err := c.renderOrderBy(sel, ti)
			if err != nil {
				return skipped, err
			}
		}

		//fmt.Fprintf(w, `), '[]') AS "%s" FROM (`, c.sel.Table)
		io.WriteString(c.w, `), '[]')`)
		alias(c.w, sel.Table)
		io.WriteString(c.w, ` FROM (`)
	}

	// ROW-TO-JSON
	io.WriteString(c.w, `SELECT `)

	if len(sel.DistinctOn) != 0 {
		c.renderDistinctOn(sel, ti)
	}

	io.WriteString(c.w, `row_to_json((`)

	//fmt.Fprintf(w, `SELECT "sel_%d" FROM (SELECT `, c.sel.ID)
	io.WriteString(c.w, `SELECT "sel_`)
	int2string(c.w, sel.ID)
	io.WriteString(c.w, `" FROM (SELECT `)

	// Combined column names
	c.renderColumns(sel, ti)

	c.renderRemoteRelColumns(sel, ti)

	err := c.renderJoinedColumns(sel, ti, skipped)
	if err != nil {
		return skipped, err
	}

	//fmt.Fprintf(w, `) AS "sel_%d"`, c.sel.ID)
	io.WriteString(c.w, `)`)
	aliasWithID(c.w, "sel", sel.ID)

	//fmt.Fprintf(w, `)) AS "%s"`, c.sel.Table)
	io.WriteString(c.w, `))`)
	aliasWithID(c.w, "sel_json", sel.ID)
	// END-ROW-TO-JSON

	if hasOrder {
		c.renderOrderByColumns(sel, ti)
	}
	// END-SELECT

	// FROM (SELECT .... )
	err = c.renderBaseSelect(sel, ti, childCols, skipped)
	if err != nil {
		return skipped, err
	}
	// END-FROM

	return skipped, nil
}

func (c *compilerContext) renderSelectClose(sel *qcode.Select, ti *DBTableInfo) error {
	hasOrder := len(sel.OrderBy) != 0

	if hasOrder {
		err := c.renderOrderBy(sel, ti)
		if err != nil {
			return err
		}
	}

	switch {
	case sel.Paging.NoLimit:
		break

	case len(sel.Paging.Limit) != 0:
		//fmt.Fprintf(w, ` LIMIT ('%s') :: integer`, c.sel.Paging.Limit)
		io.WriteString(c.w, ` LIMIT ('`)
		io.WriteString(c.w, sel.Paging.Limit)
		io.WriteString(c.w, `') :: integer`)

	case ti.Singular:
		io.WriteString(c.w, ` LIMIT ('1') :: integer`)

	default:
		io.WriteString(c.w, ` LIMIT ('20') :: integer`)
	}

	if len(sel.Paging.Offset) != 0 {
		//fmt.Fprintf(w, ` OFFSET ('%s') :: integer`, c.sel.Paging.Offset)
		io.WriteString(c.w, `OFFSET ('`)
		io.WriteString(c.w, sel.Paging.Offset)
		io.WriteString(c.w, `') :: integer`)
	}

	if ti.Singular == false {
		//fmt.Fprintf(w, `) AS "sel_json_agg_%d"`, c.sel.ID)
		io.WriteString(c.w, `)`)
		aliasWithID(c.w, "sel_json_agg", sel.ID)
	}

	return nil
}

func (c *compilerContext) renderJoin(sel *qcode.Select) error {
	io.WriteString(c.w, ` LEFT OUTER JOIN LATERAL (`)
	return nil
}

func (c *compilerContext) renderJoinClose(sel *qcode.Select) error {
	//fmt.Fprintf(w, `) AS "%s_%d_join" ON ('true')`, c.sel.Table, c.sel.ID)
	io.WriteString(c.w, `)`)
	aliasWithIDSuffix(c.w, sel.Table, sel.ID, "_join")
	io.WriteString(c.w, ` ON ('true')`)
	return nil
}

func (c *compilerContext) renderJoinTable(sel *qcode.Select) error {
	parent := &c.s[sel.ParentID]

	rel, err := c.schema.GetRel(sel.Table, parent.Table)
	if err != nil {
		return err
	}

	if rel.Type != RelOneToManyThrough {
		return err
	}

	pt, err := c.schema.GetTable(parent.Table)
	if err != nil {
		return err
	}

	//fmt.Fprintf(w, ` LEFT OUTER JOIN "%s" ON (("%s"."%s") = ("%s_%d"."%s"))`,
	//rel.Through, rel.Through, rel.ColT, c.parent.Table, c.parent.ID, rel.Col1)
	io.WriteString(c.w, ` LEFT OUTER JOIN "`)
	io.WriteString(c.w, rel.Through)
	io.WriteString(c.w, `" ON ((`)
	colWithTable(c.w, rel.Through, rel.ColT)
	io.WriteString(c.w, `) = (`)
	colWithTableID(c.w, pt.Name, parent.ID, rel.Col1)
	io.WriteString(c.w, `))`)

	return nil
}

func (c *compilerContext) renderColumns(sel *qcode.Select, ti *DBTableInfo) {
	i := 0
	for _, col := range sel.Cols {
		if len(sel.Allowed) != 0 {
			n := funcPrefixLen(col.Name)
			if n != 0 {
				if sel.Functions == false {
					continue
				}
				if _, ok := sel.Allowed[col.Name[n:]]; !ok {
					continue
				}
			} else {
				if _, ok := sel.Allowed[col.Name]; !ok {
					continue
				}
			}
		}

		if i != 0 {
			io.WriteString(c.w, ", ")
		}
		//fmt.Fprintf(w, `"%s_%d"."%s" AS "%s"`,
		//c.sel.Table, c.sel.ID, col.Name, col.FieldName)
		colWithTableIDAlias(c.w, ti.Name, sel.ID, col.Name, col.FieldName)
		i++
	}
}

func (c *compilerContext) renderRemoteRelColumns(sel *qcode.Select, ti *DBTableInfo) {
	i := 0

	for _, id := range sel.Children {
		child := &c.s[id]

		rel, err := c.schema.GetRel(child.Table, sel.Table)
		if err != nil || rel.Type != RelRemote {
			continue
		}
		if i != 0 || len(sel.Cols) != 0 {
			io.WriteString(c.w, ", ")
		}
		//fmt.Fprintf(w, `"%s_%d"."%s" AS "%s"`,
		//c.sel.Table, c.sel.ID, rel.Col1, rel.Col2)
		colWithTableID(c.w, ti.Name, sel.ID, rel.Col1)
		alias(c.w, rel.Col2)
		i++
	}
}

func (c *compilerContext) renderJoinedColumns(sel *qcode.Select, ti *DBTableInfo, skipped uint32) error {
	colsRendered := len(sel.Cols) != 0

	for _, id := range sel.Children {
		skipThis := hasBit(skipped, uint32(id))

		if colsRendered && !skipThis {
			io.WriteString(c.w, ", ")
		}
		if skipThis {
			continue
		}
		childSel := &c.s[id]

		cti, err := c.schema.GetTable(childSel.Table)
		if err != nil {
			continue
		}

		//fmt.Fprintf(w, `"%s_%d_join"."%s" AS "%s"`,
		//s.Table, s.ID, s.Table, s.FieldName)
		if cti.Singular {
			io.WriteString(c.w, `"sel_json_`)
			int2string(c.w, childSel.ID)
			io.WriteString(c.w, `" AS "`)
			io.WriteString(c.w, childSel.FieldName)
			io.WriteString(c.w, `"`)

		} else {
			colWithTableIDSuffixAlias(c.w, childSel.Table, childSel.ID,
				"_join", childSel.Table, childSel.FieldName)
		}
	}

	return nil
}

func (c *compilerContext) renderBaseSelect(sel *qcode.Select, ti *DBTableInfo,
	childCols []*qcode.Column, skipped uint32) error {
	var groupBy []int

	isRoot := sel.ID == 0
	isFil := sel.Where != nil
	isSearch := sel.Args["search"] != nil
	isAgg := false

	io.WriteString(c.w, ` FROM (SELECT `)

	i := 0
	for n, col := range sel.Cols {
		cn := col.Name

		_, isRealCol := ti.Columns[cn]

		if !isRealCol {
			if isSearch {
				switch {
				case cn == "search_rank":
					cn = ti.TSVCol
					arg := sel.Args["search"]

					if i != 0 {
						io.WriteString(c.w, `, `)
					}
					//fmt.Fprintf(w, `ts_rank("%s"."%s", to_tsquery('%s')) AS %s`,
					//c.sel.Table, cn, arg.Val, col.Name)
					io.WriteString(c.w, `ts_rank(`)
					colWithTable(c.w, ti.Name, cn)
					io.WriteString(c.w, `, to_tsquery('`)
					io.WriteString(c.w, arg.Val)
					io.WriteString(c.w, `')`)
					alias(c.w, col.Name)
					i++

				case strings.HasPrefix(cn, "search_headline_"):
					cn = cn[16:]
					arg := sel.Args["search"]

					if i != 0 {
						io.WriteString(c.w, `, `)
					}
					//fmt.Fprintf(w, `ts_headline("%s"."%s", to_tsquery('%s')) AS %s`,
					//c.sel.Table, cn, arg.Val, col.Name)
					io.WriteString(c.w, `ts_headlinek(`)
					colWithTable(c.w, ti.Name, cn)
					io.WriteString(c.w, `, to_tsquery('`)
					io.WriteString(c.w, arg.Val)
					io.WriteString(c.w, `')`)
					alias(c.w, col.Name)
					i++

				}
			} else {
				pl := funcPrefixLen(cn)
				if pl == 0 {
					if i != 0 {
						io.WriteString(c.w, `, `)
					}
					//fmt.Fprintf(w, `'%s not defined' AS %s`, cn, col.Name)
					io.WriteString(c.w, `'`)
					io.WriteString(c.w, cn)
					io.WriteString(c.w, ` not defined'`)
					alias(c.w, col.Name)
					i++

				} else if sel.Functions {
					cn1 := cn[pl:]
					if _, ok := sel.Allowed[cn1]; !ok {
						continue
					}
					if i != 0 {
						io.WriteString(c.w, `, `)
					}
					fn := cn[0 : pl-1]
					isAgg = true

					//fmt.Fprintf(w, `%s("%s"."%s") AS %s`, fn, c.sel.Table, cn, col.Name)
					io.WriteString(c.w, fn)
					io.WriteString(c.w, `(`)
					colWithTable(c.w, ti.Name, cn1)
					io.WriteString(c.w, `)`)
					alias(c.w, col.Name)
					i++

				}
			}
		} else {
			groupBy = append(groupBy, n)
			//fmt.Fprintf(w, `"%s"."%s"`, c.sel.Table, cn)
			if i != 0 {
				io.WriteString(c.w, `, `)
			}
			colWithTable(c.w, ti.Name, cn)
			i++

		}
	}

	for _, col := range childCols {
		if i != 0 {
			io.WriteString(c.w, `, `)
		}

		//fmt.Fprintf(w, `"%s"."%s"`, col.Table, col.Name)
		colWithTable(c.w, col.Table, col.Name)
		i++
	}

	io.WriteString(c.w, ` FROM `)

	//fmt.Fprintf(w, ` FROM "%s"`, c.sel.Table)
	io.WriteString(c.w, `"`)
	io.WriteString(c.w, ti.Name)
	io.WriteString(c.w, `"`)

	// if tn, ok := c.tmap[sel.Table]; ok {
	// 	//fmt.Fprintf(w, ` FROM "%s" AS "%s"`, tn, c.sel.Table)
	// 	tableWithAlias(c.w, ti.Name, sel.Table)
	// } else {
	// 	//fmt.Fprintf(w, ` FROM "%s"`, c.sel.Table)
	// 	io.WriteString(c.w, `"`)
	// 	io.WriteString(c.w, sel.Table)
	// 	io.WriteString(c.w, `"`)
	// }

	if isRoot && isFil {
		io.WriteString(c.w, ` WHERE (`)
		if err := c.renderWhere(sel, ti); err != nil {
			return err
		}
		io.WriteString(c.w, `)`)
	}

	if !isRoot {
		if err := c.renderJoinTable(sel); err != nil {
			return err
		}

		io.WriteString(c.w, ` WHERE (`)

		if err := c.renderRelationship(sel, ti); err != nil {
			return err
		}

		if isFil {
			io.WriteString(c.w, ` AND `)
			if err := c.renderWhere(sel, ti); err != nil {
				return err
			}
		}
		io.WriteString(c.w, `)`)
	}

	if isAgg {
		if len(groupBy) != 0 {
			io.WriteString(c.w, ` GROUP BY `)

			for i, id := range groupBy {
				if i != 0 {
					io.WriteString(c.w, `, `)
				}
				//fmt.Fprintf(w, `"%s"."%s"`, c.sel.Table, c.sel.Cols[id].Name)
				colWithTable(c.w, ti.Name, sel.Cols[id].Name)
			}
		}
	}

	switch {
	case sel.Paging.NoLimit:
		break

	case len(sel.Paging.Limit) != 0:
		//fmt.Fprintf(w, ` LIMIT ('%s') :: integer`, c.sel.Paging.Limit)
		io.WriteString(c.w, ` LIMIT ('`)
		io.WriteString(c.w, sel.Paging.Limit)
		io.WriteString(c.w, `') :: integer`)

	case ti.Singular:
		io.WriteString(c.w, ` LIMIT ('1') :: integer`)

	default:
		io.WriteString(c.w, ` LIMIT ('20') :: integer`)
	}

	if len(sel.Paging.Offset) != 0 {
		//fmt.Fprintf(w, ` OFFSET ('%s') :: integer`, c.sel.Paging.Offset)
		io.WriteString(c.w, ` OFFSET ('`)
		io.WriteString(c.w, sel.Paging.Offset)
		io.WriteString(c.w, `') :: integer`)
	}

	//fmt.Fprintf(w, `) AS "%s_%d"`, c.sel.Table, c.sel.ID)
	io.WriteString(c.w, `)`)
	aliasWithID(c.w, ti.Name, sel.ID)
	return nil
}

func (c *compilerContext) renderOrderByColumns(sel *qcode.Select, ti *DBTableInfo) {
	colsRendered := len(sel.Cols) != 0

	for i := range sel.OrderBy {
		if colsRendered {
			//io.WriteString(w, ", ")
			io.WriteString(c.w, `, `)
		}

		col := sel.OrderBy[i].Col
		//fmt.Fprintf(w, `"%s_%d"."%s" AS "%s_%d_%s_ob"`,
		//c.sel.Table, c.sel.ID, c,
		//c.sel.Table, c.sel.ID, c)
		colWithTableID(c.w, ti.Name, sel.ID, col)
		io.WriteString(c.w, ` AS `)
		tableIDColSuffix(c.w, sel.Table, sel.ID, col, "_ob")
	}
}

func (c *compilerContext) renderRelationship(sel *qcode.Select, ti *DBTableInfo) error {
	parent := c.s[sel.ParentID]

	rel, err := c.schema.GetRel(sel.Table, parent.Table)
	if err != nil {
		return err
	}

	switch rel.Type {
	case RelBelongTo:
		//fmt.Fprintf(w, `(("%s"."%s") = ("%s_%d"."%s"))`,
		//c.sel.Table, rel.Col1, c.parent.Table, c.parent.ID, rel.Col2)
		io.WriteString(c.w, `((`)
		colWithTable(c.w, ti.Name, rel.Col1)
		io.WriteString(c.w, `) = (`)
		colWithTableID(c.w, parent.Table, parent.ID, rel.Col2)
		io.WriteString(c.w, `))`)

	case RelOneToMany:
		//fmt.Fprintf(w, `(("%s"."%s") = ("%s_%d"."%s"))`,
		//c.sel.Table, rel.Col1, c.parent.Table, c.parent.ID, rel.Col2)
		io.WriteString(c.w, `((`)
		colWithTable(c.w, ti.Name, rel.Col1)
		io.WriteString(c.w, `) = (`)
		colWithTableID(c.w, parent.Table, parent.ID, rel.Col2)
		io.WriteString(c.w, `))`)

	case RelOneToManyThrough:
		//fmt.Fprintf(w, `(("%s"."%s") = ("%s"."%s"))`,
		//c.sel.Table, rel.Col1, rel.Through, rel.Col2)
		io.WriteString(c.w, `((`)
		colWithTable(c.w, ti.Name, rel.Col1)
		io.WriteString(c.w, `) = (`)
		colWithTable(c.w, rel.Through, rel.Col2)
		io.WriteString(c.w, `))`)
	}

	return nil
}

func (c *compilerContext) renderWhere(sel *qcode.Select, ti *DBTableInfo) error {
	st := util.NewStack()

	if sel.Where != nil {
		st.Push(sel.Where)
	}

	for {
		if st.Len() == 0 {
			break
		}

		intf := st.Pop()

		switch val := intf.(type) {
		case qcode.ExpOp:
			switch val {
			case qcode.OpAnd:
				io.WriteString(c.w, ` AND `)
			case qcode.OpOr:
				io.WriteString(c.w, ` OR `)
			case qcode.OpNot:
				io.WriteString(c.w, `NOT `)
			default:
				return fmt.Errorf("11: unexpected value %v (%t)", intf, intf)
			}

		case *qcode.Exp:
			switch val.Op {
			case qcode.OpAnd, qcode.OpOr:
				for i := len(val.Children) - 1; i >= 0; i-- {
					st.Push(val.Children[i])
					if i > 0 {
						st.Push(val.Op)
					}
				}
				qcode.FreeExp(val)

			case qcode.OpNot:
				st.Push(val.Children[0])
				st.Push(qcode.OpNot)
				qcode.FreeExp(val)

			default:
				if val.NestedCol {
					//fmt.Fprintf(w, `(("%s") `, val.Col)
					io.WriteString(c.w, `(("`)
					io.WriteString(c.w, val.Col)
					io.WriteString(c.w, `") `)

				} else if len(val.Col) != 0 {
					//fmt.Fprintf(w, `(("%s"."%s") `, c.sel.Table, val.Col)
					io.WriteString(c.w, `((`)
					colWithTable(c.w, ti.Name, val.Col)
					io.WriteString(c.w, `) `)
				}
				valExists := true

				switch val.Op {
				case qcode.OpEquals:
					io.WriteString(c.w, `=`)
				case qcode.OpNotEquals:
					io.WriteString(c.w, `!=`)
				case qcode.OpGreaterOrEquals:
					io.WriteString(c.w, `>=`)
				case qcode.OpLesserOrEquals:
					io.WriteString(c.w, `<=`)
				case qcode.OpGreaterThan:
					io.WriteString(c.w, `>`)
				case qcode.OpLesserThan:
					io.WriteString(c.w, `<`)
				case qcode.OpIn:
					io.WriteString(c.w, `IN`)
				case qcode.OpNotIn:
					io.WriteString(c.w, `NOT IN`)
				case qcode.OpLike:
					io.WriteString(c.w, `LIKE`)
				case qcode.OpNotLike:
					io.WriteString(c.w, `NOT LIKE`)
				case qcode.OpILike:
					io.WriteString(c.w, `ILIKE`)
				case qcode.OpNotILike:
					io.WriteString(c.w, `NOT ILIKE`)
				case qcode.OpSimilar:
					io.WriteString(c.w, `SIMILAR TO`)
				case qcode.OpNotSimilar:
					io.WriteString(c.w, `NOT SIMILAR TO`)
				case qcode.OpContains:
					io.WriteString(c.w, `@>`)
				case qcode.OpContainedIn:
					io.WriteString(c.w, `<@`)
				case qcode.OpHasKey:
					io.WriteString(c.w, `?`)
				case qcode.OpHasKeyAny:
					io.WriteString(c.w, `?|`)
				case qcode.OpHasKeyAll:
					io.WriteString(c.w, `?&`)
				case qcode.OpIsNull:
					if strings.EqualFold(val.Val, "true") {
						io.WriteString(c.w, `IS NULL)`)
					} else {
						io.WriteString(c.w, `IS NOT NULL)`)
					}
					valExists = false
				case qcode.OpEqID:
					if len(ti.PrimaryCol) == 0 {
						return fmt.Errorf("no primary key column defined for %s", ti.Name)
					}
					//fmt.Fprintf(w, `(("%s") =`, c.ti.PrimaryCol)
					io.WriteString(c.w, `((`)
					colWithTable(c.w, ti.Name, ti.PrimaryCol)
					//io.WriteString(c.w, ti.PrimaryCol)
					io.WriteString(c.w, `) =`)
				case qcode.OpTsQuery:
					if len(ti.TSVCol) == 0 {
						return fmt.Errorf("no tsv column defined for %s", ti.Name)
					}
					//fmt.Fprintf(w, `(("%s") @@ to_tsquery('%s'))`, c.ti.TSVCol, val.Val)
					io.WriteString(c.w, `(("`)
					io.WriteString(c.w, ti.TSVCol)
					io.WriteString(c.w, `") @@ to_tsquery('`)
					io.WriteString(c.w, val.Val)
					io.WriteString(c.w, `'))`)
					valExists = false

				default:
					return fmt.Errorf("[Where] unexpected op code %d", val.Op)
				}

				if valExists {
					if val.Type == qcode.ValList {
						c.renderList(val)
					} else {
						c.renderVal(val, c.vars)
					}
					io.WriteString(c.w, `)`)
				}

				qcode.FreeExp(val)
			}

		default:
			return fmt.Errorf("12: unexpected value %v (%t)", intf, intf)
		}

	}

	return nil
}

func (c *compilerContext) renderOrderBy(sel *qcode.Select, ti *DBTableInfo) error {
	io.WriteString(c.w, ` ORDER BY `)
	for i := range sel.OrderBy {
		if i != 0 {
			io.WriteString(c.w, `, `)
		}
		ob := sel.OrderBy[i]

		switch ob.Order {
		case qcode.OrderAsc:
			//fmt.Fprintf(w, `"%s_%d.ob.%s" ASC`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` ASC`)
		case qcode.OrderDesc:
			//fmt.Fprintf(w, `"%s_%d.ob.%s" DESC`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` DESC`)
		case qcode.OrderAscNullsFirst:
			//fmt.Fprintf(w, `"%s_%d.ob.%s" ASC NULLS FIRST`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` ASC NULLS FIRST`)
		case qcode.OrderDescNullsFirst:
			//fmt.Fprintf(w, `%s_%d.ob.%s DESC NULLS FIRST`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` DESC NULLLS FIRST`)
		case qcode.OrderAscNullsLast:
			//fmt.Fprintf(w, `"%s_%d.ob.%s ASC NULLS LAST`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` ASC NULLS LAST`)
		case qcode.OrderDescNullsLast:
			//fmt.Fprintf(w, `%s_%d.ob.%s DESC NULLS LAST`, sel.Table, sel.ID, ob.Col)
			tableIDColSuffix(c.w, ti.Name, sel.ID, ob.Col, "_ob")
			io.WriteString(c.w, ` DESC NULLS LAST`)
		default:
			return fmt.Errorf("13: unexpected value %v", ob.Order)
		}
	}
	return nil
}

func (c *compilerContext) renderDistinctOn(sel *qcode.Select, ti *DBTableInfo) {
	io.WriteString(c.w, `DISTINCT ON (`)
	for i := range sel.DistinctOn {
		if i != 0 {
			io.WriteString(c.w, `, `)
		}
		//fmt.Fprintf(w, `"%s_%d.ob.%s"`, c.sel.Table, c.sel.ID, c.sel.DistinctOn[i])
		tableIDColSuffix(c.w, ti.Name, sel.ID, sel.DistinctOn[i], "_ob")
	}
	io.WriteString(c.w, `) `)
}

func (c *compilerContext) renderList(ex *qcode.Exp) {
	io.WriteString(c.w, ` (`)
	for i := range ex.ListVal {
		if i != 0 {
			io.WriteString(c.w, `, `)
		}
		switch ex.ListType {
		case qcode.ValBool, qcode.ValInt, qcode.ValFloat:
			io.WriteString(c.w, ex.ListVal[i])
		case qcode.ValStr:
			io.WriteString(c.w, `'`)
			io.WriteString(c.w, ex.ListVal[i])
			io.WriteString(c.w, `'`)
		}
	}
	io.WriteString(c.w, `)`)
}

func (c *compilerContext) renderVal(ex *qcode.Exp, vars map[string]string) {
	io.WriteString(c.w, ` `)

	switch ex.Type {
	case qcode.ValBool, qcode.ValInt, qcode.ValFloat:
		if len(ex.Val) != 0 {
			io.WriteString(c.w, ex.Val)
		} else {
			io.WriteString(c.w, `''`)
		}

	case qcode.ValStr:
		io.WriteString(c.w, `'`)
		io.WriteString(c.w, ex.Val)
		io.WriteString(c.w, `'`)

	case qcode.ValVar:
		if val, ok := vars[ex.Val]; ok {
			io.WriteString(c.w, val)
		} else {
			//fmt.Fprintf(w, `'{{%s}}'`, ex.Val)
			io.WriteString(c.w, `{{`)
			io.WriteString(c.w, ex.Val)
			io.WriteString(c.w, `}}`)
		}
	}
	//io.WriteString(c.w, `)`)
}

func funcPrefixLen(fn string) int {
	switch {
	case strings.HasPrefix(fn, "avg_"):
		return 4
	case strings.HasPrefix(fn, "count_"):
		return 6
	case strings.HasPrefix(fn, "max_"):
		return 4
	case strings.HasPrefix(fn, "min_"):
		return 4
	case strings.HasPrefix(fn, "sum_"):
		return 4
	case strings.HasPrefix(fn, "stddev_"):
		return 7
	case strings.HasPrefix(fn, "stddev_pop_"):
		return 11
	case strings.HasPrefix(fn, "stddev_samp_"):
		return 12
	case strings.HasPrefix(fn, "variance_"):
		return 9
	case strings.HasPrefix(fn, "var_pop_"):
		return 8
	case strings.HasPrefix(fn, "var_samp_"):
		return 9
	}
	return 0
}

func hasBit(n uint32, pos uint32) bool {
	val := n & (1 << pos)
	return (val > 0)
}

func alias(w io.Writer, alias string) {
	io.WriteString(w, ` AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `"`)
}

func aliasWithID(w io.Writer, alias string, id int32) {
	io.WriteString(w, ` AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, `"`)
}

func aliasWithIDSuffix(w io.Writer, alias string, id int32, suffix string) {
	io.WriteString(w, ` AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, suffix)
	io.WriteString(w, `"`)
}

func colWithAlias(w io.Writer, col, alias string) {
	io.WriteString(w, `"`)
	io.WriteString(w, col)
	io.WriteString(w, `" AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `"`)
}

func tableWithAlias(w io.Writer, table, alias string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `" AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `"`)
}

func colWithTable(w io.Writer, table, col string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `"."`)
	io.WriteString(w, col)
	io.WriteString(w, `"`)
}

func colWithTableID(w io.Writer, table string, id int32, col string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, `"."`)
	io.WriteString(w, col)
	io.WriteString(w, `"`)
}

func colWithTableIDAlias(w io.Writer, table string, id int32, col, alias string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, `"."`)
	io.WriteString(w, col)
	io.WriteString(w, `" AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `"`)
}

func colWithTableIDSuffixAlias(w io.Writer, table string, id int32,
	suffix, col, alias string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, suffix)
	io.WriteString(w, `"."`)
	io.WriteString(w, col)
	io.WriteString(w, `" AS "`)
	io.WriteString(w, alias)
	io.WriteString(w, `"`)
}

func tableIDColSuffix(w io.Writer, table string, id int32, col, suffix string) {
	io.WriteString(w, `"`)
	io.WriteString(w, table)
	io.WriteString(w, `_`)
	int2string(w, id)
	io.WriteString(w, `_`)
	io.WriteString(w, col)
	io.WriteString(w, suffix)
	io.WriteString(w, `"`)
}

const charset = "0123456789"

func int2string(w io.Writer, val int32) {
	if val < 10 {
		w.Write([]byte{charset[val]})
		return
	}

	temp := int32(0)
	val2 := val
	for val2 > 0 {
		temp *= 10
		temp += val2 % 10
		val2 = int32(math.Floor(float64(val2 / 10)))
	}

	val3 := temp
	for val3 > 0 {
		d := val3 % 10
		val3 /= 10
		w.Write([]byte{charset[d]})
	}
}

func relID(h *xxhash.Digest, child, parent string) uint64 {
	h.WriteString(child)
	h.WriteString(parent)
	v := h.Sum64()
	h.Reset()
	return v
}