// Package digitraffic reads live Finnish cargo rail from Fintraffic's open API.
//
// The upstream is a real public GraphQL endpoint and needs no key, which is why
// this service has no credential to leak and no quota to exhaust.
package digitraffic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/calemccammon/freight-alerts/internal/rules"
)

const (
	DefaultEndpoint = "https://rata.digitraffic.fi/api/v2/graphql/graphql"

	// Fintraffic asks callers to identify themselves. It is not authentication
	// and not required, but sending it is the courtesy the API's terms request.
	userAgentHeader = "Digitraffic-User"
	userAgentValue  = "calemccammon/freight-alerts"
)

// cargoTrainsQuery asks only for the fields the rules package consumes.
// timeTableRows is where the delay lives: differenceInMinutes is positive when
// late, and actualTime is present only once the train has actually reached the
// stop. Both are needed to distinguish a realised delay from an estimate.
const cargoTrainsQuery = `{
  currentlyRunningTrains(where: {trainType: {trainCategory: {name: {equals: "Cargo"}}}}) {
    trainNumber
    departureDate
    operator { shortCode }
    timeTableRows {
      station { name }
      type
      scheduledTime
      actualTime
      differenceInMinutes
    }
  }
}`

type Client struct {
	endpoint string
	http     *http.Client
}

func New(endpoint string, timeout time.Duration) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: timeout}}
}

type graphQLResponse struct {
	Data struct {
		CurrentlyRunningTrains []struct {
			TrainNumber   int    `json:"trainNumber"`
			DepartureDate string `json:"departureDate"`
			Operator      struct {
				ShortCode string `json:"shortCode"`
			} `json:"operator"`
			TimeTableRows []struct {
				Station struct {
					Name string `json:"name"`
				} `json:"station"`
				Type                string     `json:"type"`
				ScheduledTime       time.Time  `json:"scheduledTime"`
				ActualTime          *time.Time `json:"actualTime"`
				DifferenceInMinutes *int       `json:"differenceInMinutes"`
			} `json:"timeTableRows"`
		} `json:"currentlyRunningTrains"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// RunningCargoTrains returns every cargo train currently running.
func (c *Client) RunningCargoTrains(ctx context.Context) ([]rules.Train, error) {
	body, err := json.Marshal(map[string]string{"query": cargoTrainsQuery})
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(userAgentHeader, userAgentValue)
	// Accept-Encoding is deliberately not set. Digitraffic answers 406 without
	// gzip, and Go's transport adds the header and decompresses transparently --
	// but only while the caller leaves it alone. Setting it by hand hands
	// decompression back to us and breaks the response body.

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var parsed graphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// GraphQL reports failures in the body with a 200 status, so a successful
	// HTTP call is not a successful query.
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", parsed.Errors[0].Message)
	}

	trains := make([]rules.Train, 0, len(parsed.Data.CurrentlyRunningTrains))
	for _, t := range parsed.Data.CurrentlyRunningTrains {
		train := rules.Train{
			TrainNumber:   t.TrainNumber,
			DepartureDate: t.DepartureDate,
			Operator:      t.Operator.ShortCode,
			Rows:          make([]rules.TimetableRow, 0, len(t.TimeTableRows)),
		}
		for _, r := range t.TimeTableRows {
			train.Rows = append(train.Rows, rules.TimetableRow{
				Station:             r.Station.Name,
				Type:                r.Type,
				ScheduledTime:       r.ScheduledTime,
				ActualTime:          r.ActualTime,
				DifferenceInMinutes: r.DifferenceInMinutes,
			})
		}
		trains = append(trains, train)
	}
	return trains, nil
}
