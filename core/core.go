package core

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/dosco/graphjin/core/internal/psql"
	"github.com/dosco/graphjin/core/internal/qcode"
	"github.com/dosco/graphjin/core/internal/sdata"
	"github.com/jackc/pgx/v4/pgxpool"
)

type OpType int

const (
	OpUnknown OpType = iota
	OpQuery
	OpSubscription
	OpMutation
)

type extensions struct {
	Tracing *trace `json:"tracing,omitempty"`
}

type trace struct {
	Version   int           `json:"version"`
	StartTime time.Time     `json:"startTime"`
	EndTime   time.Time     `json:"endTime"`
	Duration  time.Duration `json:"duration"`
	Execution execution     `json:"execution"`
}

type execution struct {
	Resolvers []resolver `json:"resolvers"`
}

type resolver struct {
	Path        []string      `json:"path"`
	ParentType  string        `json:"parentType"`
	FieldName   string        `json:"fieldName"`
	ReturnType  string        `json:"returnType"`
	StartOffset int           `json:"startOffset"`
	Duration    time.Duration `json:"duration"`
}

type gcontext struct {
	context.Context

	gj   *graphjin
	op   qcode.QType
	rc   *ReqConfig
	name string
}

type queryResp struct {
	qc   *queryComp
	data []byte
	role string
}

func (gj *graphjin) initDiscover() error {
	switch gj.conf.DBType {
	case "":
		gj.dbtype = "postgres"
	case "mssql":
		gj.dbtype = "mysql"
	default:
		gj.dbtype = gj.conf.DBType
	}

	if err := gj._initDiscover(); err != nil {
		return fmt.Errorf("%s: %w", gj.dbtype, err)
	}
	return nil
}

func (gj *graphjin) _initDiscover() error {
	var err error

	// If gj.dbinfo is not null then it's probably set
	// for tests
	if gj.dbinfo == nil {
		gj.dbinfo, err = sdata.GetDBInfo(
			gj.pool,
			gj.dbtype,
			gj.conf.Blocklist)
	}

	return err
}

func (gj *graphjin) initSchema() error {
	if err := gj._initSchema(); err != nil {
		return fmt.Errorf("%s: %w", gj.dbtype, err)
	}
	return nil
}

func (gj *graphjin) _initSchema() error {
	var err error

	if len(gj.dbinfo.Tables) == 0 {
		return fmt.Errorf("no tables found in database")
	}

	schema := gj.dbinfo.Schema
	for i, t := range gj.conf.Tables {
		if t.Schema == "" {
			gj.conf.Tables[i].Schema = schema
			t.Schema = schema
		}
		// skip aliases
		if t.Table != "" && t.Type == "" {
			continue
		}
		if err := addTableInfo(gj.conf, t); err != nil {
			return err
		}
	}

	if err := addTables(gj.conf, gj.dbinfo); err != nil {
		return err
	}

	if err := addForeignKeys(gj.conf, gj.dbinfo); err != nil {
		return err
	}

	gj.schema, err = sdata.NewDBSchema(
		gj.dbinfo,
		getDBTableAliases(gj.conf))

	ssufx := gj.conf.SingularSuffix
	if ssufx == "" {
		ssufx = "ByID"
	}
	gj.schema.SingularSuffix = ssufx

	return err
}

func (gj *graphjin) initCompilers() error {
	var err error

	qcc := qcode.Config{
		TConfig:          gj.conf.tmap,
		DefaultBlock:     gj.conf.DefaultBlock,
		DefaultLimit:     gj.conf.DefaultLimit,
		DisableAgg:       gj.conf.DisableAgg,
		DisableFuncs:     gj.conf.DisableFuncs,
		EnableCamelcase:  gj.conf.EnableCamelcase,
		DBSchema:         gj.schema.DBSchema(),
	}

	if gj.allowList != nil {
		qcc.FragmentFetcher = gj.allowList.FragmentFetcher()
	}

	gj.qc, err = qcode.NewCompiler(gj.schema, qcc)
	if err != nil {
		return err
	}

	if err := addRoles(gj.conf, gj.qc); err != nil {
		return err
	}

	gj.pc = psql.NewCompiler(psql.Config{
		Vars:      gj.conf.Vars,
		DBType:    gj.schema.DBType(),
		DBVersion: gj.schema.DBVersion(),
	})
	return nil
}

func (gj *graphjin) executeRoleQuery(c context.Context, conn *pgxpool.Conn, md psql.Metadata, vars []byte, rc *ReqConfig) (string, error) {
	var role string
	var ar args
	var err error

	if conn == nil {
		if conn, err = gj.pool.Acquire(c); err != nil {
			return role, err
		}
		defer conn.Release()
	}

	if c.Value(UserIDKey) == nil {
		return "anon", nil
	}

	if ar, err = gj.argList(c, md, vars, rc); err != nil {
		return "", err
	}

	err = conn.QueryRow(c, gj.roleStmt, ar.values...).Scan(&role)
	return role, err
}

func (c *gcontext) execQuery(qr queryReq, role string) (queryResp, error) {
	res, err := c.resolveSQL(qr, role)
	if err != nil {
		return res, err
	}

	if c.gj.conf.Debug {
		c.debugLog(&res.qc.st)
	}

	qc := res.qc.st.qc

	if len(res.data) == 0 {
		return res, nil
	}

	if qc.Remotes != 0 {
		if res, err = c.execRemoteJoin(res); err != nil {
			return res, err
		}
	}

	return res, err
}

func (c *gcontext) resolveSQL(qr queryReq, role string) (queryResp, error) {
	var res queryResp
	var err error

	res.role = role

	conn, err := c.gj.pool.Acquire(c)
	if err != nil {
		return res, err
	}
	defer conn.Release()

	if c.gj.conf.SetUserID {
		if err := c.setLocalUserID(conn); err != nil {
			return res, err
		}
	}

	if v := c.Value(UserRoleKey); v != nil {
		res.role = v.(string)

	} else if c.gj.abacEnabled {
		res.role, err = c.gj.executeRoleQuery(c, conn, c.gj.roleStmtMD, qr.vars, c.rc)
	}

	if err != nil {
		return res, err
	}

	if res.qc, err = c.gj.compileQuery(qr, res.role); err != nil {
		return res, err
	}

	args, err := c.gj.argList(c, res.qc.st.md, qr.vars, c.rc)
	if err != nil {
		return res, err
	}

	// var stime time.Time

	// if c.gj.conf.EnableTracing {
	// 	stime = time.Now()
	// }

	rows, err := conn.Query(c, res.qc.st.sql, args.values...)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	if rows.Next() {
		values := rows.RawValues()
		if len(values) != 0 {
			res.data = values[0]
			if c.rc != nil && c.rc.APQKey != "" {
				c.gj.apq.Set(c.rc.APQKey, apqInfo{op: qr.op, name: qr.name, query: string(qr.query)})
			}
			return res, nil
		}
	}

	return res, sql.ErrNoRows
}

func (c *gcontext) setLocalUserID(conn *pgxpool.Conn) error {
	var err error

	if v := c.Value(UserIDKey); v == nil {
		return nil
	} else {
		switch v1 := v.(type) {
		case string:
			_, err = conn.Exec(c, `SET SESSION "user.id" = '`+v1+`'`)

		case int:
			_, err = conn.Exec(c, `SET SESSION "user.id" = `+strconv.Itoa(v1))
		}
	}

	return err
}

func (r *Result) Operation() OpType {
	switch r.op {
	case qcode.QTQuery:
		return OpQuery

	case qcode.QTMutation, qcode.QTInsert, qcode.QTUpdate, qcode.QTUpsert, qcode.QTDelete:
		return OpMutation

	default:
		return -1
	}
}

func (r *Result) OperationName() string {
	return r.op.String()
}

func (r *Result) QueryName() string {
	return r.name
}

func (r *Result) Role() string {
	return r.role
}

func (r *Result) SQL() string {
	return r.sql
}

func (r *Result) CacheControl() string {
	return r.cacheControl
}

// func (c *gcontext) addTrace(sel []qcode.Select, id int32, st time.Time) {
// 	et := time.Now()
// 	du := et.Sub(st)

// 	if c.res.Extensions == nil {
// 		c.res.Extensions = &extensions{&trace{
// 			Version:   1,
// 			StartTime: st,
// 			Execution: execution{},
// 		}}
// 	}

// 	c.res.Extensions.Tracing.EndTime = et
// 	c.res.Extensions.Tracing.Duration = du

// 	n := 1
// 	for i := id; i != -1; i = sel[i].ParentID {
// 		n++
// 	}
// 	path := make([]string, n)

// 	n--
// 	for i := id; ; i = sel[i].ParentID {
// 		path[n] = sel[i].Name
// 		if sel[i].ParentID == -1 {
// 			break
// 		}
// 		n--
// 	}

// 	tr := resolver{
// 		Path:        path,
// 		ParentType:  "Query",
// 		FieldName:   sel[id].Name,
// 		ReturnType:  "object",
// 		StartOffset: 1,
// 		Duration:    du,
// 	}

// 	c.res.Extensions.Tracing.Execution.Resolvers =
// 		append(c.res.Extensions.Tracing.Execution.Resolvers, tr)
// }

func (c *gcontext) debugLog(st *stmt) {
	for _, sel := range st.qc.Selects {
		if sel.SkipRender == qcode.SkipTypeUserNeeded {
			c.gj.log.Printf("Field skipped, requires $user_id or table not added to anon role: %s", sel.FieldName)
		}
	}
}
