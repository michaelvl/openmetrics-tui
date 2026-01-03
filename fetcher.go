package main

import (
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	promModel "github.com/prometheus/common/model"
)

type Fetcher struct {
	URL string
}

func NewFetcher(url string) *Fetcher {
	return &Fetcher{URL: url}
}

func (f *Fetcher) Fetch() (map[string]*dto.MetricFamily, error) {
	resp, err := http.Get(f.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	parser := expfmt.NewTextParser(promModel.UTF8Validation)
	return parser.TextToMetricFamilies(resp.Body)
}
