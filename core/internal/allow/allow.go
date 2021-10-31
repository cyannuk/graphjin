package allow

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/scanner"

	"gopkg.in/yaml.v3"
)

const (
	expComment = iota + 1
	expVar
	expQuery
	expFrag
)

const (
	queryPath    = "/queries"
	fragmentPath = "/fragments"
)

type Item struct {
	Name     string
	Comment  string `yaml:",omitempty"`
	key      string
	Query    string
	Vars     string   `yaml:",omitempty"`
	Metadata Metadata `yaml:",inline,omitempty"`
	frags    []Frag
}

type Metadata struct {
	Order struct {
		Var    string   `yaml:"var,omitempty"`
		Values []string `yaml:"values,omitempty"`
	} `yaml:",omitempty"`
}

type Frag struct {
	Name  string
	Value string
}

type List struct {
	dir string
}

func New(dir string) (*List) {
	return &List{dir}
}

func (al *List) Load() ([]Item, error) {
	var items []Item

	files, err := os.ReadDir(path.Join(al.dir, queryPath))
	if err != nil {
		return nil, fmt.Errorf("allow list: %w", err)
	}

	for _, f := range files {
		var item Item
		//var mi bool

		fn := f.Name()
		fn = strings.TrimSuffix(fn, filepath.Ext(fn))

		newFile := path.Join(al.dir, queryPath, fn + ".yaml")
		b, err := os.ReadFile(newFile)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, &item); err != nil {
			return nil, err
		}

		items = append(items, item)
	}

	return items, nil
}

func parseQuery(b string) (Item, error) {
	var s scanner.Scanner
	s.Init(strings.NewReader(b))
	s.Mode ^= scanner.SkipComments

	var op, sp scanner.Position
	var item Item
	var err error

	st := expComment

	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		txt := s.TokenText()

		switch {
		case strings.HasPrefix(txt, "/*"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = s.Pos()

		case strings.HasPrefix(txt, "variables"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = s.Pos()
			st = expVar

		case isGraphQL(txt):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = op
			st = expQuery

		case strings.HasPrefix(txt, "fragment"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = op
			st = expFrag
		}

		if err != nil {
			return item, err
		}

		op = s.Pos()
	}

	if st == expQuery || st == expFrag {
		v := b[sp.Offset:s.Pos().Offset]
		item, err = setValue(st, v, item)
	}

	if err != nil {
		return item, err
	}

	item.Name = QueryName(item.Query)
	item.key = strings.ToLower(item.Name)
	return item, nil
}

func setValue(st int, v string, item Item) (Item, error) {
	val := func() string {
		return strings.TrimSpace(v[:strings.LastIndexByte(v, '}')+1])
	}
	switch st {
	case expComment:
		item.Comment = val()

	case expVar:
		item.Vars = val()

	case expQuery:
		item.Query = val()

	case expFrag:
		f := Frag{Value: val()}
		f.Name = QueryName(f.Value)
		item.frags = append(item.frags, f)
	}

	return item, nil
}

func (al *List) FragmentFetcher() func(name string) (string, error) {
	return func(name string) (string, error) {
		v, err := os.ReadFile(path.Join(fragmentPath, name))
		return string(v), err
	}
}

// func (al *List) GetQuery(name string) (Item, error) {
// 	var item Item
// 	var err error

// 	b, err := ioutil.ReadFile(path.Join(al.queryPath, (name + ".yaml")))
// 	if err == nil {
// 		return item, err
// 	}

// 	return parseYAML(b)
// }

// func parseYAML(b []byte) (Item, error) {
// 	var item Item
// 	err := yaml.Unmarshal(b, &item)
// 	return item, err
// }
