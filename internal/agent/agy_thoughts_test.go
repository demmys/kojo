package agent

import (
	"database/sql"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
)

func protoTestLen(num uint64, v []byte) []byte {
	b := binary.AppendUvarint(nil, num<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func protoTestVarint(num, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, num<<3), v)
}

func agyTestPayload(thought string) []byte {
	var inner []byte
	inner = append(inner, protoTestLen(1, []byte("other"))...)
	inner = append(inner, protoTestLen(3, []byte(thought))...)
	var p []byte
	p = append(p, protoTestVarint(1, 15)...)
	p = append(p, protoTestLen(5, []byte{0x08, 0x01})...)
	p = append(p, protoTestLen(20, inner)...)
	return p
}

func TestExtractAgyThoughtFromPayload(t *testing.T) {
	if got := extractAgyThoughtFromPayload(agyTestPayload("思考 summary")); got != "思考 summary" {
		t.Fatalf("got %q", got)
	}
	noThought := append(protoTestVarint(1, 15), protoTestLen(20, protoTestLen(1, []byte("x")))...)
	for name, p := range map[string][]byte{
		"empty":       nil,
		"no field 20": protoTestVarint(1, 15),
		"no sub 3":    noThought,
		"truncated":   agyTestPayload("hello")[:10],
		"bad length":  {0xa2, 0x01, 0xff, 0xff, 0x7f},
		"bad utf8":    protoTestLen(20, protoTestLen(3, []byte{0xff, 0xfe})),
	} {
		if got := extractAgyThoughtFromPayload(p); got != "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestAgyThoughtReaderAndStream(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, agyIDA+".db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx integer PRIMARY KEY, step_type integer NOT NULL DEFAULT 0, step_payload blob)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps VALUES (3, 15, ?), (1, 132, ?)`, agyTestPayload("pick apple"), agyTestPayload("not a response")); err != nil {
		t.Fatal(err)
	}
	db.Close()

	r := &agyThoughtReader{dir: dir, logger: silentLogger()}
	defer r.Close()
	if got, ok := r.Lookup(agyIDA, 3); !ok || got != "pick apple" {
		t.Fatalf("Lookup(3) = %q, %v", got, ok)
	}
	if _, ok := r.Lookup(agyIDA, 1); ok {
		t.Fatal("non agent_response step must not match")
	}
	if _, ok := r.Lookup("missing", 3); ok {
		t.Fatal("missing store must not match")
	}

	var events []ChatEvent
	res := parseAgyStream(strings.NewReader(agyTestStream), silentLogger(), func(e ChatEvent) bool {
		events = append(events, e)
		return true
	}, nil, r.Lookup)
	if res.thinking != "pick apple" {
		t.Fatalf("thinking = %q", res.thinking)
	}
	thinkAt, textAt := -1, -1
	for i, e := range events {
		if e.Type == "thinking" {
			if thinkAt >= 0 {
				t.Fatal("thinking sent twice")
			}
			thinkAt = i
		}
		if e.Type == "text" && textAt < 0 {
			textAt = i
		}
	}
	if thinkAt < 0 || thinkAt > textAt {
		t.Fatalf("thinking at %d, first text at %d", thinkAt, textAt)
	}
}
