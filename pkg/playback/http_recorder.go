package playback

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
)

// A recordServer proxies requests to a remote host, and records all interactions.
type recordServer struct {
	recorder   *interactionRecorder
	remoteURL  string
	webhookURL string
}

func newRecordServer(remoteURL string, webhookURL string) (httpRecorder recordServer) {
	httpRecorder = recordServer{}
	httpRecorder.remoteURL = remoteURL
	httpRecorder.webhookURL = webhookURL

	return httpRecorder
}

func (httpRecorder *recordServer) insertCassette(writer io.Writer) error {
	recorder, err := newInteractionRecorder(writer, httpRequestToBytes, httpResponseToBytes)
	if err != nil {
		return err
	}
	httpRecorder.recorder = recorder

	return nil
}

func (httpRecorder *recordServer) webhookHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("\n[WEBHOOK] --> STRIPE %v to %v --> POST to %v", r.Method, r.RequestURI, httpRecorder.webhookURL)

	// --- Pass request to local webhook endpoint
	resp, err := forwardRequest(r, httpRecorder.webhookURL)

	// TODO: this response is going back to Stripe, what is the correct error handling logic here?
	if err != nil {
		handleErrorInHandler(w, fmt.Errorf("Unexpected error forwarding webhook to client: %w", err), 500)
		return
	}

	// --- Write response back to client
	// The header *must* be written first, since writing the body with implicitly and irreversibly set
	// the status code to 200 if not already set.
	// Copy header
	w.WriteHeader(resp.StatusCode)
	copyHTTPHeader(w.Header(), resp.Header)

	// Copy body
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		handleErrorInHandler(w, fmt.Errorf("Unexpected error processing client webhook response: %w", err), 500)
		return
	}
	io.Copy(w, bytes.NewBuffer(bodyBytes))
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	defer resp.Body.Close()

	err = httpRecorder.recorder.write(incomingInteraction, r, resp)
	if err != nil {
		handleErrorInHandler(w, fmt.Errorf("Unexpected error writing webhook interaction to cassette: %w", err), 500)
		return
	}
}

func (httpRecorder *recordServer) handler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("\n--> %v to %v", r.Method, r.RequestURI)

	// --- Pass request to remote
	var resp *http.Response
	var err error

	resp, err = forwardRequest(r, httpRecorder.remoteURL+r.RequestURI)

	if err != nil {
		handleErrorInHandler(w, err, 500)
		return
	}
	fmt.Printf("\n<-- %v from %v\n", resp.Status, strings.ToUpper(httpRecorder.remoteURL))

	// --- Write response back to client
	// The header *must* be written first, since writing the body with implicitly and irreversibly set
	// the status code to 200 if not already set.
	// Copy header
	w.WriteHeader(resp.StatusCode)
	copyHTTPHeader(w.Header(), resp.Header)

	// Copy body
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close() // we need to close the body

	if err != nil {
		handleErrorInHandler(w, err, 500)
		return
	}
	io.Copy(w, bytes.NewBuffer(bodyBytes))
	// we need to reset the reader to be able to read the body again
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	defer resp.Body.Close()

	err = httpRecorder.recorder.write(outgoingInteraction, r, resp)
	if err != nil {
		handleErrorInHandler(w, err, 500)
		return
	}

	// Reset the body reader in case we add code later that performs another read
	resp.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	defer resp.Body.Close()
}

func (httpRecorder *recordServer) initializeServer(address string) *http.Server {
	customMux := http.NewServeMux()
	server := &http.Server{Addr: address, Handler: customMux}

	// --- Recorder control handlers
	customMux.HandleFunc("/playback/stop", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println()
		fmt.Println("Received /playback/stop. Stopping...")

		httpRecorder.recorder.saveAndClose()
	})

	// --- Default handler that proxies request and returns response from the remote
	customMux.HandleFunc("/", httpRecorder.handler)

	return server
}