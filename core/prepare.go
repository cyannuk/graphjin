package core

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/dosco/graphjin/core/internal/allow"
	"github.com/dosco/graphjin/core/internal/qcode"
)

type queryReq struct {
	op    qcode.QType
	name  string
	query []byte
	vars  []byte
	order [2]string
}

// nolint: errcheck
func (gj *graphjin) prepareRoleStmt() error {
	if !gj.abacEnabled {
		return nil
	}

	if !strings.Contains(gj.conf.RolesQuery, "$user_id") {
		return fmt.Errorf("roles_query: $user_id variable missing")
	}

	w := &bytes.Buffer{}

	io.WriteString(w, `SELECT (CASE WHEN EXISTS (`)
	gj.pc.RenderVar(w, &gj.roleStmtMD, gj.conf.RolesQuery)
	io.WriteString(w, `) THEN `)

	io.WriteString(w, `(SELECT (CASE`)
	for roleName, role := range gj.roles {
		if role.Match == "" {
			continue
		}
		io.WriteString(w, ` WHEN `)
		io.WriteString(w, role.Match)
		io.WriteString(w, ` THEN '`)
		io.WriteString(w, roleName)
		io.WriteString(w, `'`)
	}

	io.WriteString(w, ` ELSE 'user' END) FROM (`)
	gj.pc.RenderVar(w, &gj.roleStmtMD, gj.conf.RolesQuery)
	io.WriteString(w, `) AS "_sg_auth_roles_query" LIMIT 1) `)
	io.WriteString(w, `ELSE 'anon' END) FROM (VALUES (1)) AS "_sg_auth_filler" LIMIT 1; `)

	gj.roleStmt = w.String()

	return nil
}

func (gj *graphjin) initAllowList() error {
	var err error

	if gj.conf.DisableAllowList {
		return nil
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	baseDir := path.Join(workingDir, "allow-list")
	queryDir := path.Join(baseDir, "queries")
	if _, err := os.Stat(queryDir); os.IsNotExist(err) {
		return fmt.Errorf("dir not found: %s", queryDir)
	}
	gj.allowList = allow.New(baseDir)

	if gj.allowList == nil {
		return nil
	}

	gj.queries = make(map[string]*queryComp)

	list, err := gj.allowList.Load()
	if err != nil {
		return err
	}

	for _, item := range list {
		if item.Query == "" {
			continue
		}

		qt, _ := qcode.GetQType(item.Query)
		qk := gj.getQueryKeys(item)

		for _, v := range qk {
			qc := &queryComp{qr: queryReq{
				op:    qt,
				name:  item.Name,
				query: []byte(item.Query),
				vars:  []byte(item.Vars),
			}}

			if item.Metadata.Order.Var != "" {
				qc.qr.order = [2]string{item.Metadata.Order.Var, strconv.Quote(v.val)}
			}
			gj.queries[v.key] = qc
		}

		op, _ := qcode.GetQType(item.Query)
		gj.apq.Set(item.Name, apqInfo{op: op, name: item.Name})
	}

	return nil
}

type queryKey struct {
	key string
	val string
}

func (gj *graphjin) getQueryKeys(item allow.Item) []queryKey {
	var qk []queryKey

	for roleName := range gj.roles {
		qk = append(qk, queryKey{key: (item.Name + roleName)})

		for _, v := range item.Metadata.Order.Values {
			qk = append(qk, queryKey{key: (item.Name + roleName + v), val: v})
		}
	}
	return qk
}
