package digitraffic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A trimmed copy of a real response, including the two shapes that matter: a
// stop the train has reached (actualTime + differenceInMinutes present) and one
// it has not (both absent).
const sampleResponse = `{"data":{"currentlyRunningTrains":[
  {"trainNumber":2011,"departureDate":"2026-08-16","operator":{"shortCode":"vr"},
   "timeTableRows":[
     {"station":{"name":"Kouvola"},"type":"DEPARTURE","scheduledTime":"2026-08-16T13:10:00.000Z","actualTime":"2026-08-16T13:12:00.000Z","differenceInMinutes":2},
     {"station":{"name":"Taavetti"},"type":"DEPARTURE","scheduledTime":"2026-08-16T14:52:00.000Z","actualTime":"2026-08-16T15:00:39.000Z","differenceInMinutes":9},
     {"station":{"name":"Lappeenranta"},"type":"ARRIVAL","scheduledTime":"2026-08-16T15:40:00.000Z","actualTime":null,"differenceInMinutes":null}
   ]}
]}}`

func serverReturning(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, 5*time.Second), captured
}

func TestParsesTrainsAndTimetableRows(t *testing.T) {
	client, _ := serverReturning(t, http.StatusOK, sampleResponse)

	trains, err := client.RunningCargoTrains(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(trains) != 1 {
		t.Fatalf("got %d trains, want 1", len(trains))
	}
	train := trains[0]
	if train.TrainNumber != 2011 || train.Operator != "vr" || train.DepartureDate != "2026-08-16" {
		t.Fatalf("unexpected train header: %+v", train)
	}
	if len(train.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(train.Rows))
	}
}

func TestAnUnreachedStopParsesAsNotRealised(t *testing.T) {
	// The distinction the rules package depends on: a null actualTime is an
	// estimate, not a delay that has happened.
	client, _ := serverReturning(t, http.StatusOK, sampleResponse)
	trains, err := client.RunningCargoTrains(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows := trains[0].Rows
	if !rows[1].Realised() {
		t.Fatal("a stop with actualTime should be realised")
	}
	if rows[2].Realised() {
		t.Fatal("a stop with a null actualTime must not be realised")
	}
}

func TestCurrentDelayComesFromTheLastReachedStop(t *testing.T) {
	client, _ := serverReturning(t, http.StatusOK, sampleResponse)
	trains, _ := client.RunningCargoTrains(context.Background())
	delay, station, ok := trains[0].CurrentDelay()
	if !ok || delay != 9 || station != "Taavetti" {
		t.Fatalf("got (%d, %q, %v), want (9, Taavetti, true)", delay, station, ok)
	}
}

func TestSendsTheCourtesyIdentificationHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(userAgentHeader)
		_, _ = w.Write([]byte(sampleResponse))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 5*time.Second).RunningCargoTrains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != userAgentValue {
		t.Fatalf("Digitraffic-User = %q, want %q", got, userAgentValue)
	}
}

func TestDoesNotSetAcceptEncodingItself(t *testing.T) {
	// Setting it by hand disables Go's transparent gzip handling, and
	// Digitraffic answers 406 without gzip -- so the header must be left to the
	// transport. This pins that it stays that way.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Accept-Encoding")
		_, _ = w.Write([]byte(sampleResponse))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, 5*time.Second).RunningCargoTrains(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "gzip") {
		t.Fatalf("Accept-Encoding = %q, expected the transport to request gzip", got)
	}
}

func TestAnHTTPErrorIsReported(t *testing.T) {
	client, _ := serverReturning(t, http.StatusServiceUnavailable, "upstream down")
	_, err := client.RunningCargoTrains(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected a 503 to be reported, got %v", err)
	}
}

func TestAGraphQLErrorIsNotTreatedAsSuccess(t *testing.T) {
	// GraphQL reports failures in the body with a 200 status, so a successful
	// HTTP call is not a successful query.
	client, _ := serverReturning(t, http.StatusOK,
		`{"errors":[{"message":"BadFaithIntrospection"}]}`)
	_, err := client.RunningCargoTrains(context.Background())
	if err == nil || !strings.Contains(err.Error(), "BadFaithIntrospection") {
		t.Fatalf("expected the GraphQL error to surface, got %v", err)
	}
}

func TestMalformedJSONIsReported(t *testing.T) {
	client, _ := serverReturning(t, http.StatusOK, "{not json")
	if _, err := client.RunningCargoTrains(context.Background()); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestAnEmptyFleetIsNotAnError(t *testing.T) {
	// Finnish freight rail genuinely goes quiet overnight; zero trains is a
	// normal answer, not a failure.
	client, _ := serverReturning(t, http.StatusOK, `{"data":{"currentlyRunningTrains":[]}}`)
	trains, err := client.RunningCargoTrains(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(trains) != 0 {
		t.Fatalf("got %d trains, want 0", len(trains))
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := New(srv.URL, 5*time.Second).RunningCargoTrains(ctx); err == nil {
		t.Fatal("expected cancellation to abort the request")
	}
}
