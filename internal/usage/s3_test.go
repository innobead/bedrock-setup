package usage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/parquet-go/parquet-go"
)

type fakeS3 struct {
	objects map[string][]byte
	gets    int
	bytes   int
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	for k, b := range f.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), Size: aws.Int64(int64(len(b)))})
		}
	}
	return out, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b := f.objects[aws.ToString(in.Key)]
	var from, to int
	if _, err := fmt.Sscanf(aws.ToString(in.Range), "bytes=%d-%d", &from, &to); err != nil {
		return nil, err
	}
	if to >= len(b) {
		to = len(b) - 1
	}
	f.gets++
	f.bytes += to - from + 1
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b[from : to+1]))}, nil
}

func TestS3Source(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixtures, "cur-this-month.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeS3{objects: map[string][]byte{
		"cur/bedrock-cur/data/BILLING_PERIOD=2026-10/part-0.parquet": b,
		"cur/bedrock-cur/data/BILLING_PERIOD=2026-10/manifest.json":  []byte("{}"),
		"cur/bedrock-cur/data/BILLING_PERIOD=2026-09/part-0.parquet": b,
	}}
	src := &S3Source{S3: f, Bucket: "bkt", Prefix: "/cur/bedrock-cur/"}
	if w := src.Where("2026-10"); w != "s3://bkt/cur/bedrock-cur/data/BILLING_PERIOD=2026-10/" {
		t.Fatal(w)
	}
	lines := readAll(t, src, "2026-10")
	if len(lines) != 13 || f.gets == 0 {
		t.Fatalf("lines %d gets %d", len(lines), f.gets)
	}
}

func TestTimestampUnits(t *testing.T) {
	type micros struct {
		Principal string    `parquet:"line_item_iam_principal"`
		Cost      float64   `parquet:"line_item_unblended_cost"`
		Start     time.Time `parquet:"line_item_usage_start_date,timestamp(microsecond)"`
	}
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[micros](&buf)
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := w.Write([]micros{{Principal: iamUser("u"), Cost: 1.5, Start: want}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var got []Line
	if err := ReadFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()), func(l Line) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Start.Equal(want) || got[0].Cost != 1.5 || got[0].Owner != "" || got[0].Model != "" {
		t.Fatalf("%+v", got)
	}
}

func TestNotACostExport(t *testing.T) {
	type other struct {
		A string `parquet:"a"`
	}
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[other](&buf)
	_, _ = w.Write([]other{{"x"}})
	_ = w.Close()
	err := ReadFile(bytes.NewReader(buf.Bytes()), int64(buf.Len()), func(Line) {})
	if err == nil || !strings.Contains(err.Error(), "CUR 2.0") {
		t.Fatal(err)
	}
}
