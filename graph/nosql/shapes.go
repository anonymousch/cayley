package nosql

import (
	"fmt"
	"math"
	"strconv"

	"github.com/cayleygraph/cayley/graph"
	"github.com/cayleygraph/cayley/graph/iterator"
	"github.com/cayleygraph/cayley/quad"
	"github.com/cayleygraph/cayley/query/shape"
	"github.com/cayleygraph/cayley/query/shape/gshape"
)

var _ shape.Optimizer = (*QuadStore)(nil)

func (qs *QuadStore) OptimizeShape(s shape.Shape) (shape.Shape, bool) {
	switch s := s.(type) {
	case gshape.Quads:
		return qs.optimizeQuads(s)
	case shape.Filter:
		return qs.optimizeFilter(s)
	case shape.Page:
		return qs.optimizePage(s)
	case shape.Composite:
		if s2, opt := s.Simplify().Optimize(qs); opt {
			return s2, true
		}
	}
	return s, false
}

// Shape is a shape representing a documents query with filters
type Shape struct {
	Collection string        // name of the collection
	Filters    []FieldFilter // filters to select documents
	Limit      int64         // limits a number of documents
}

func (s Shape) BuildIterator(qs graph.QuadStore) iterator.Iterator {
	db, ok := qs.(*QuadStore)
	if !ok {
		return iterator.NewError(fmt.Errorf("not a nosql database: %T", qs))
	}
	return NewIterator(db, s.Collection, s.Filters...)
}

func (s Shape) Optimize(r shape.Optimizer) (shape.Shape, bool) {
	return s, false
}

// Quads is a shape representing a quads query
type Quads struct {
	Links []Linkage // filters to select quads
	Limit int64     // limits a number of documents
}

func (s Quads) BuildIterator(qs graph.QuadStore) iterator.Iterator {
	db, ok := qs.(*QuadStore)
	if !ok {
		return iterator.NewError(fmt.Errorf("not a nosql database: %T", qs))
	}
	return NewLinksToIterator(db, colQuads, s.Links)
}

func (s Quads) Optimize(r shape.Optimizer) (shape.Shape, bool) {
	return s, false
}

const int64Adjust = 1 << 63

// itos serializes int64 into a sortable string 13 chars long.
func itos(i int64) string {
	s := strconv.FormatUint(uint64(i)+int64Adjust, 32)
	const z = "0000000000000"
	return z[len(s):] + s
}

// stoi de-serializes int64 from a sortable string 13 chars long.
func stoi(s string) int64 {
	ret, err := strconv.ParseUint(s, 32, 64)
	if err != nil {
		//TODO handle error?
		return 0
	}
	return int64(ret - int64Adjust)
}

func (opt Options) toFieldFilter(c shape.Comparison) ([]FieldFilter, bool) {
	var op FilterOp
	switch c.Op {
	case shape.CompareEQ:
		op = Equal
	case shape.CompareNEQ:
		op = NotEqual
	case shape.CompareGT:
		op = GT
	case shape.CompareGTE:
		op = GTE
	case shape.CompareLT:
		op = LT
	case shape.CompareLTE:
		op = LTE
	default:
		return nil, false
	}
	fieldPath := func(s string) []string {
		return []string{fldValue, s}
	}

	var filters []FieldFilter
	switch v := c.Val.(type) {
	case quad.String:
		filters = []FieldFilter{
			{Path: fieldPath(fldValData), Filter: op, Value: String(v)},
			{Path: fieldPath(fldIRI), Filter: NotEqual, Value: Bool(true)},
			{Path: fieldPath(fldBNode), Filter: NotEqual, Value: Bool(true)},
		}
	case quad.IRI:
		filters = []FieldFilter{
			{Path: fieldPath(fldValData), Filter: op, Value: String(v)},
			{Path: fieldPath(fldIRI), Filter: Equal, Value: Bool(true)},
		}
	case quad.BNode:
		filters = []FieldFilter{
			{Path: fieldPath(fldValData), Filter: op, Value: String(v)},
			{Path: fieldPath(fldBNode), Filter: Equal, Value: Bool(true)},
		}
	case quad.Int:
		if opt.Number32 && (v < math.MinInt32 || v > math.MaxInt32) {
			// switch to range on string values
			filters = []FieldFilter{
				{Path: fieldPath(fldValStrInt), Filter: op, Value: String(itos(int64(v)))},
			}
		} else {
			filters = []FieldFilter{
				{Path: fieldPath(fldValInt), Filter: op, Value: Int(v)},
			}
		}
	case quad.Float:
		filters = []FieldFilter{
			{Path: fieldPath(fldValFloat), Filter: op, Value: Float(v)},
		}
	case quad.Time:
		filters = []FieldFilter{
			{Path: fieldPath(fldValTime), Filter: op, Value: Time(v)},
		}
	default:
		return nil, false
	}
	return filters, true
}

func (qs *QuadStore) optimizeFilter(s shape.Filter) (shape.Shape, bool) {
	if _, ok := s.From.(gshape.AllNodes); !ok {
		return s, false
	}
	var (
		filters []FieldFilter
		left    []shape.ValueFilter
	)
	fieldPath := func(s string) []string {
		return []string{fldValue, s}
	}
	for _, f := range s.Filters {
		switch f := f.(type) {
		case shape.Comparison:
			if fld, ok := qs.opt.toFieldFilter(f); ok {
				filters = append(filters, fld...)
				continue
			}
		case shape.Wildcard:
			filters = append(filters, []FieldFilter{
				{Path: fieldPath(fldValData), Filter: Regexp, Value: String(f.Regexp())},
			}...)
			continue
		case shape.Regexp:
			filters = append(filters, []FieldFilter{
				{Path: fieldPath(fldValData), Filter: Regexp, Value: String(f.Re.String())},
			}...)
			if !f.Refs {
				filters = append(filters, []FieldFilter{
					{Path: fieldPath(fldIRI), Filter: NotEqual, Value: Bool(true)},
					{Path: fieldPath(fldBNode), Filter: NotEqual, Value: Bool(true)},
				}...)
			}
			continue
		}
		left = append(left, f)
	}
	if len(filters) == 0 {
		return s, false
	}
	var ns shape.Shape = Shape{Collection: colNodes, Filters: filters}
	if len(left) != 0 {
		ns = shape.Filter{From: ns, Filters: left}
	}
	return ns, true
}

func (qs *QuadStore) optimizeQuads(s gshape.Quads) (shape.Shape, bool) {
	var (
		links []Linkage
		left  []gshape.QuadFilter
	)
	for _, f := range s {
		if v, ok := gshape.One(f.Values); ok {
			if h, ok := v.(NodeHash); ok {
				links = append(links, Linkage{Dir: f.Dir, Val: h})
				continue
			}
		}
		left = append(left, f)
	}
	if len(links) == 0 {
		return s, false
	}
	var ns shape.Shape = Quads{Links: links}
	if len(left) != 0 {
		ns = gshape.Intersect{ns, gshape.Quads(left)}
	}
	return s, true
}

func (qs *QuadStore) optimizePage(s shape.Page) (shape.Shape, bool) {
	if s.Skip != 0 {
		return s, false
	}
	switch f := s.From.(type) {
	case gshape.AllNodes:
		return Shape{Collection: colNodes, Limit: s.Limit}, false
	case Shape:
		s.ApplyPage(shape.Page{Limit: f.Limit})
		f.Limit = s.Limit
		return f, true
	case Quads:
		s.ApplyPage(shape.Page{Limit: f.Limit})
		f.Limit = s.Limit
		return f, true
	}
	return s, false
}
