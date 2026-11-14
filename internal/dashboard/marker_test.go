package dashboard

import "testing"

func TestThresholdMarker(t *testing.T) {
	for _, c := range []struct {
		th   Threshold
		want string
	}{
		{Threshold{Color: "red"}, "▲"},
		{Threshold{Color: "orange"}, "◆"},
		{Threshold{Color: "yellow"}, "■"},
		{Threshold{Color: "green"}, "●"},
		{Threshold{Color: "#ff8800"}, "●"},
		{Threshold{Color: "red", Symbol: "✗"}, "✗"},
	} {
		if got := c.th.Marker(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.th, got, c.want)
		}
	}
}
