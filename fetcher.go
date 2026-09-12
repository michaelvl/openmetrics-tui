package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	promModel "github.com/prometheus/common/model"
)

// acceptHeader advertises only the formats expfmt can actually decode, protobuf
// first. OpenMetrics is deliberately left out: expfmt.ResponseFormat cannot
// identify it, so an OpenMetrics response would silently land on the text
// fallback path.
var acceptHeader = string(expfmt.NewFormat(expfmt.TypeProtoDelim)) + ";q=0.9, " +
	string(expfmt.NewFormat(expfmt.TypeTextPlain)) + ";q=0.8, */*;q=0.1"

type Fetcher struct {
	URL    string
	client *http.Client
}

func NewFetcher(url string) *Fetcher {
	return &Fetcher{
		URL: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (f *Fetcher) Fetch() (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequest(http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	// The escaping scheme keeps metric name validation on UTF-8 rather than the
	// stricter legacy rules NewDecoder would otherwise pick.
	format := expfmt.ResponseFormat(resp.Header).WithEscapingScheme(promModel.NoEscaping)
	dec := expfmt.NewDecoder(resp.Body, format)

	families := make(map[string]*dto.MetricFamily)
	for {
		var family dto.MetricFamily
		err := dec.Decode(&family)
		if errors.Is(err, io.EOF) {
			return families, nil
		}
		if err != nil {
			return nil, err
		}
		families[family.GetName()] = &family
	}
}
