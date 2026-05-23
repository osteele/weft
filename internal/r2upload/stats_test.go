package r2upload

import "testing"

func TestExtractBytes(t *testing.T) {
	cases := []struct {
		line string
		want int64
		ok   bool
	}{
		{`{"level":"notice","msg":"Transferred: 1.234 MiB / 10.000 MiB, 12%, 1.2 MiB/s, ETA 1m13s"}`, 1293942, true}, // 1.234 * 1<<20 = 1293942.78... truncated to int64
		{`{"level":"notice","msg":"Transferred: 12 GiB / 100 GiB, 12%, 100 MiB/s"}`, 12 * (1 << 30), true},
		{`Transferred: 100 B / 200 B, 50%`, 100, true},
		{`Transferred: 1.5 KB / 10 KB, 15%`, 1536, true}, // 1.5 * 1024
		// non-stats lines:
		{`{"level":"info","msg":"Copying file foo.txt"}`, 0, false},
		{`Some unrelated line`, 0, false},
		{``, 0, false},
		{`{"level":"notice","msg":"Transferred: 0 / 5, 0%"}`, 0, true}, // bare-bytes form with no unit
	}
	for _, c := range cases {
		got, ok := extractBytes(c.line)
		if ok != c.ok {
			t.Errorf("line=%q: ok=%v want %v", c.line, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("line=%q: got=%d want=%d", c.line, got, c.want)
		}
	}
}
