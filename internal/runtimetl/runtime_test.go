package runtimetl

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
)

const published = `// LAYER 234
item#123 data:long = Item;
result#456 flags:# yes:flags.1?true items:Vector<Item> text:flags.2?string = Result;
---functions---
example.invoke#789 flags:# input:Item text:flags.2?string other:flags.2?int = Result;
invokeWithoutUpdates#bf9459b7 {X:Type} query:!X = X;
`

func TestRuntimeRPCEncodingAndDecoding(t *testing.T) {
	s, err := Parse([]byte(published))
	if err != nil {
		t.Fatal(err)
	}
	call, err := s.Prepare("invokeWithoutUpdates",
		[]byte(`{"query":{"_":"example.invoke","input":{"_":"item","data":"9007199254740993"},"text":"hello","other":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	var wire, expected bin.Buffer
	expected.PutID(0xbf9459b7)
	expected.PutID(0x789)
	expected.PutUint32(4)
	expected.PutID(0x123)
	expected.PutLong(9007199254740993)
	expected.PutString("hello")
	expected.PutInt32(7)
	if err := call.Encode(&wire); err != nil || !bytes.Equal(wire.Buf, expected.Buf) {
		t.Fatalf("wrong encoded request: %x", wire.Buf)
	}
	wire.Reset()
	wire.PutID(0x456)
	wire.PutUint32(2)
	wire.PutVectorHeader(1)
	wire.PutID(0x123)
	wire.PutLong(math.MinInt64)
	if err := call.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	decoded := call.Response.(map[string]any)
	if decoded["yes"] != true ||
		decoded["items"].([]any)[0].(map[string]any)["data"] != "-9223372036854775808" {
		t.Fatalf("unexpected result: %#v", decoded)
	}
	if _, err := s.Prepare("example.invoke", []byte(`{"input":{"_":"item","data":1},"text":"missing partner"}`)); err == nil {
		t.Fatal("accepted incomplete shared flag fields")
	}
	if _, err := s.Prepare("example.invoke", []byte(`{"input":{"_":"example.invoke","data":1}}`)); err == nil {
		t.Fatal("accepted a method where the schema expects a constructor")
	}
}

func TestRefreshIntroducesMethodAndKeepsLastGoodSchema(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "telegram-api.tl")
	now := time.Now()
	current := published
	fetch := func(context.Context) ([]byte, error) { return []byte(current), nil }
	s, err := refresh(t.Context(), cache, now, fetch)
	if err != nil || s.Layer != 234 {
		t.Fatalf("initial refresh: %v", err)
	}
	current = strings.Replace(published, "234", "235", 1) + "example.future#abc = Bool;\n"
	s, err = refresh(t.Context(), cache, now.Add(6*time.Minute), fetch)
	if err != nil || s.Layer != 235 || !slices.Contains(s.Names(), "example.future") {
		t.Fatalf("updating schema: %v", err)
	}
	if _, err := s.Prepare("example.future", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	for i, old := range []string{published, "unsupported schema"} {
		current = old
		s, err = refresh(t.Context(), cache, now.Add(time.Duration(12+i*6)*time.Minute), fetch)
		if err != nil || s.Layer != 235 {
			t.Fatalf("downgraded the last valid schema: %v", err)
		}
	}
	s, err = refresh(t.Context(), cache, now.Add(24*time.Minute),
		func(context.Context) ([]byte, error) { return nil, errors.New("offline") })
	if err != nil || s.Layer != 235 {
		t.Fatalf("lost last valid schema when offline: %v", err)
	}
	st, err := os.Stat(cache)
	if err != nil || st.Mode().Perm()&0077 != 0 {
		t.Fatalf("cache permissions: %v", err)
	}
	if _, err := refresh(t.Context(), cache, now.Add(25*time.Minute),
		func(context.Context) ([]byte, error) {
			t.Fatal("re-fetched within the failed refresh retry window")
			return nil, nil
		}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshWithoutWritableCache(t *testing.T) {
	path := t.TempDir() // A directory cannot be atomically replaced by the schema file.
	s, err := refresh(t.Context(), path, time.Now(),
		func(context.Context) ([]byte, error) { return []byte(published), nil })
	if err != nil || s.Layer != 234 {
		t.Fatalf("fetched schema should work without a cache: %v", err)
	}
}
