// Package usage reads the CUR 2.0 cost export (Parquet) and reports Bedrock spend per caller.
package usage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

// Line is one cost export row with a caller: a model call (or a batch of them).
type Line struct {
	Principal string // line_item_iam_principal: the caller's ARN
	Owner     string // tags['iamPrincipal/owner']
	Cost      float64
	Start     time.Time
	Model     string // product['product_name'], else line_item_usage_type, else the product code
}

const (
	colPrincipal = "line_item_iam_principal"
	colCost      = "line_item_unblended_cost"
	colStart     = "line_item_usage_start_date"
	colUsageType = "line_item_usage_type"
	colCode      = "line_item_product_code"
	colTags      = "tags"
	colProduct   = "product"
	ownerTag     = "iamPrincipal/owner"
)

// Object is one export file.
type Object struct {
	Key  string
	Size int64
}

// Source lists and opens a month's export files.
type Source interface {
	List(ctx context.Context, month string) ([]Object, error)
	Open(ctx context.Context, o Object) (io.ReaderAt, error)
	Where(month string) string
}

// S3API is the part of the S3 client the reader uses.
type S3API interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// S3Source reads s3://Bucket/Prefix/data/BILLING_PERIOD=YYYY-MM/*.parquet, where Prefix is
// <prefix>/<export name>.
type S3Source struct {
	S3     S3API
	Bucket string
	Prefix string
}

func (s *S3Source) dir(month string) string {
	return strings.Trim(s.Prefix, "/") + "/data/BILLING_PERIOD=" + month + "/"
}

func (s *S3Source) Where(month string) string { return "s3://" + s.Bucket + "/" + s.dir(month) }

func (s *S3Source) List(ctx context.Context, month string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.S3, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(s.dir(month))})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			if k := aws.ToString(o.Key); strings.HasSuffix(k, ".parquet") {
				out = append(out, Object{Key: k, Size: aws.ToInt64(o.Size)})
			}
		}
	}
	return out, nil
}

func (s *S3Source) Open(ctx context.Context, o Object) (io.ReaderAt, error) {
	return &rangeReader{ctx: ctx, s3: s.S3, bucket: s.Bucket, key: o.Key}, nil
}

// rangeReader reads byte ranges with GET requests, so only the footer and the needed columns are
// downloaded.
type rangeReader struct {
	ctx         context.Context
	s3          S3API
	bucket, key string
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	out, err := r.s3.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket), Key: aws.String(r.key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)),
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Body.Close() }()
	n, err := io.ReadFull(out.Body, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

// DirSource reads Dir/data/BILLING_PERIOD=YYYY-MM/*.parquet (a local copy of the export).
type DirSource struct{ Dir string }

func (d DirSource) dir(month string) string {
	return filepath.Join(d.Dir, "data", "BILLING_PERIOD="+month)
}

func (d DirSource) Where(month string) string { return d.dir(month) }

func (d DirSource) List(_ context.Context, month string) ([]Object, error) {
	paths, err := filepath.Glob(filepath.Join(d.dir(month), "*.parquet"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Object
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		out = append(out, Object{Key: p, Size: st.Size()})
	}
	return out, nil
}

func (d DirSource) Open(_ context.Context, o Object) (io.ReaderAt, error) {
	b, err := os.ReadFile(o.Key)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(string(b)), nil
}

// ReadMonth calls fn for every row with a caller in the month's export files and returns the
// number of files read.
func ReadMonth(ctx context.Context, src Source, month string, fn func(Line)) (int, error) {
	objs, err := src.List(ctx, month)
	if err != nil {
		return 0, err
	}
	for _, o := range objs {
		r, err := src.Open(ctx, o)
		if err != nil {
			return 0, err
		}
		if err := ReadFile(r, o.Size, fn); err != nil {
			return 0, fmt.Errorf("%s: %w", o.Key, err)
		}
	}
	return len(objs), nil
}

// ReadFile calls fn for every row of one Parquet file that has a caller (line_item_iam_principal).
// It reads only the columns it needs, one row group at a time.
func ReadFile(r io.ReaderAt, size int64, fn func(Line)) error {
	f, err := parquet.OpenFile(r, size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true), parquet.ReadBufferSize(1<<20))
	if err != nil {
		return err
	}
	schema := f.Schema()
	need := func(name string) (parquet.LeafColumn, error) {
		leaf, ok := schema.Lookup(name)
		if !ok {
			return leaf, fmt.Errorf("column %s not found; is this a CUR 2.0 export?", name)
		}
		return leaf, nil
	}
	principal, err := need(colPrincipal)
	if err != nil {
		return err
	}
	cost, err := need(colCost)
	if err != nil {
		return err
	}
	start, err := need(colStart)
	if err != nil {
		return err
	}
	usageType, hasUsageType := schema.Lookup(colUsageType)
	code, hasCode := schema.Lookup(colCode)
	tagK, tagV, hasTags := mapLeaves(schema, colTags)
	prodK, prodV, hasProduct := mapLeaves(schema, colProduct)
	startConv := timeConverter(start)

	for _, rg := range f.RowGroups() {
		chunks := rg.ColumnChunks()
		read := func(leaf parquet.LeafColumn) ([][]parquet.Value, error) {
			return readColumn(chunks[leaf.ColumnIndex])
		}
		pr, err := read(principal)
		if err != nil {
			return err
		}
		// Skip the other columns when no row in this group has a caller.
		found := false
		for _, v := range pr {
			if v[0].DefinitionLevel() == principal.MaxDefinitionLevel && len(v[0].ByteArray()) > 0 {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		co, err := read(cost)
		if err != nil {
			return err
		}
		st, err := read(start)
		if err != nil {
			return err
		}
		var ut, pc, tk, tv, pk, pv [][]parquet.Value
		if hasUsageType {
			if ut, err = read(usageType); err != nil {
				return err
			}
		}
		if hasCode {
			if pc, err = read(code); err != nil {
				return err
			}
		}
		if hasTags {
			if tk, err = read(tagK); err != nil {
				return err
			}
			if tv, err = read(tagV); err != nil {
				return err
			}
		}
		if hasProduct {
			if pk, err = read(prodK); err != nil {
				return err
			}
			if pv, err = read(prodV); err != nil {
				return err
			}
		}
		for i, v := range pr {
			arn := str(v[0], principal)
			if arn == "" {
				continue
			}
			l := Line{Principal: arn, Cost: num(co[i][0], cost), Start: startConv(st[i][0])}
			if hasTags {
				l.Owner = lookup(tk[i], tv[i], tagK, tagV, ownerTag)
			}
			if hasProduct {
				l.Model = lookup(pk[i], pv[i], prodK, prodV, "product_name")
			}
			if l.Model == "" && hasUsageType {
				l.Model = str(ut[i][0], usageType)
			}
			if l.Model == "" && hasCode {
				l.Model = str(pc[i][0], code)
			}
			fn(l)
		}
	}
	return nil
}

// mapLeaves finds the key and value leaves of a MAP column.
func mapLeaves(s *parquet.Schema, name string) (k, v parquet.LeafColumn, ok bool) {
	var kp, vp []string
	for _, path := range s.Columns() {
		if len(path) == 3 && path[0] == name {
			switch path[2] {
			case "key":
				kp = path
			case "value":
				vp = path
			}
		}
	}
	if kp == nil || vp == nil {
		return k, v, false
	}
	k, ok1 := s.Lookup(kp...)
	v, ok2 := s.Lookup(vp...)
	return k, v, ok1 && ok2
}

// readColumn returns the values of one column chunk grouped by row (a repetition level of 0 starts
// a row). Null values are kept, so map keys and values stay aligned.
func readColumn(cc parquet.ColumnChunk) ([][]parquet.Value, error) {
	pages := cc.Pages()
	defer func() { _ = pages.Close() }()
	var rows [][]parquet.Value
	buf := make([]parquet.Value, 1024)
	for {
		page, err := pages.ReadPage()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		vr := page.Values()
		for {
			n, err := vr.ReadValues(buf)
			for _, v := range buf[:n] {
				v = v.Clone()
				if v.RepetitionLevel() == 0 || len(rows) == 0 {
					rows = append(rows, []parquet.Value{v})
				} else {
					rows[len(rows)-1] = append(rows[len(rows)-1], v)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				parquet.Release(page)
				return nil, err
			}
		}
		parquet.Release(page)
	}
}

func defined(v parquet.Value, leaf parquet.LeafColumn) bool {
	return !v.IsNull() && v.DefinitionLevel() == leaf.MaxDefinitionLevel
}

func str(v parquet.Value, leaf parquet.LeafColumn) string {
	if !defined(v, leaf) {
		return ""
	}
	return string(v.ByteArray())
}

func num(v parquet.Value, leaf parquet.LeafColumn) float64 {
	if !defined(v, leaf) {
		return 0
	}
	switch v.Kind() {
	case parquet.Double:
		return v.Double()
	case parquet.Float:
		return float64(v.Float())
	case parquet.ByteArray, parquet.FixedLenByteArray:
		f, _ := strconv.ParseFloat(string(v.ByteArray()), 64)
		return f
	case parquet.Int64:
		return float64(v.Int64())
	case parquet.Int32:
		return float64(v.Int32())
	}
	return 0
}

func lookup(keys, vals []parquet.Value, kl, vl parquet.LeafColumn, want string) string {
	for j, k := range keys {
		if j < len(vals) && str(k, kl) == want {
			return str(vals[j], vl)
		}
	}
	return ""
}

// timeConverter reads timestamps stored as INT64 (with any unit), INT96 or strings.
func timeConverter(leaf parquet.LeafColumn) func(parquet.Value) time.Time {
	unit := time.Duration(0)
	if lt := leaf.Node.Type().LogicalType(); lt != nil {
		if ts, ok := lt.Value.(*format.TimestampType); ok && ts.Unit.Value != nil {
			unit = ts.Unit.Value.Duration()
		}
	}
	return func(v parquet.Value) time.Time {
		if !defined(v, leaf) {
			return time.Time{}
		}
		switch v.Kind() {
		case parquet.Int64:
			n := v.Int64()
			u := unit
			if u == 0 { // no annotation: guess from the magnitude
				switch {
				case n > 1e17:
					u = time.Nanosecond
				case n > 1e14:
					u = time.Microsecond
				default:
					u = time.Millisecond
				}
			}
			return time.Unix(0, n*int64(u)).UTC()
		case parquet.Int96:
			i := v.Int96()
			nanos := binary.LittleEndian.Uint64([]byte{
				byte(i[0]), byte(i[0] >> 8), byte(i[0] >> 16), byte(i[0] >> 24),
				byte(i[1]), byte(i[1] >> 8), byte(i[1] >> 16), byte(i[1] >> 24)})
			days := int64(i[2]) - 2440588 // Julian day of 1970-01-01
			return time.Unix(days*86400, int64(nanos)).UTC()
		case parquet.ByteArray:
			s := string(v.ByteArray())
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
				if t, err := time.Parse(layout, s); err == nil {
					return t.UTC()
				}
			}
		}
		return time.Time{}
	}
}
