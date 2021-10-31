package core

import (
	"github.com/dosco/graphjin/core/internal/qcode"
	"github.com/dosco/graphjin/internal/jsn"
)

type cursors struct {
	data  []byte
	value string
}

func (gj *graphjin) getCursor(qc *qcode.QCode, data []byte) (cursors, error) {
	var keys [][]byte
	cur := cursors{data: data}

	for _, sel := range qc.Selects {
		if sel.Paging.Cursor {
			keys = append(keys, []byte((sel.FieldName + "_cursor")))
		}
	}

	if len(keys) == 0 {
		return cur, nil
	}

	for _, f := range jsn.Get(data, keys) {
		if f.Value[0] != '"' || f.Value[len(f.Value)-1] != '"' {
			continue
		}

		if len(f.Value) > 2 {
			// save a copy of the first cursor value to use
			// with subscriptions when fetching the next set
			if cur.value == "" {
				cur.value = string(f.Value[1 : len(f.Value)-1])
			}
		}
	}

	return cur, nil
}
