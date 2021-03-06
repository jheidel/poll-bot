package main

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	server = flag.String("server", "wss://www.jeffheidel.com/poll", "Path to control server")
	port   = flag.Int("port", 8080, "Path for health check server")

	BuildTimestamp string
	BuildGitHash   string

	rng = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func Version() string {
	return fmt.Sprintf("poll-bot{ts=%s, h=%s}", BuildTimestamp, BuildGitHash)
}

type PollRequest struct {
	URLs     []string
	Method   string
	Header   http.Header
	Body     string
	Interval time.Duration
	Jitter   time.Duration
}

type PollResponse struct {
	FromURL      string
	Hash         string
	Status       string
	StatusCode   int
	Header       http.Header
	Body         string
	PollVersion  string
	PollHostname string
}

func topLevelContext() context.Context {
	ctx, cancelf := context.WithCancel(context.Background())
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigs
		log.Warnf("Caught signal %q, shutting down.", sig)
		cancelf()
	}()
	return ctx
}

type Poller struct {
	conn   *websocket.Conn
	client *http.Client

	request  *PollRequest
	idx      int
	lastHash []string
}

func (p *Poller) DoPoll(ctx context.Context) error {
	if p.client == nil {
		p.client = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	idx := p.idx
	url := p.request.URLs[idx]
	p.idx++
	if p.idx >= len(p.request.URLs) {
		p.idx = 0
	}

	log.Infof("Polling %v", url)
	req, err := http.NewRequestWithContext(ctx, p.request.Method, url, strings.NewReader(p.request.Body))
	if err != nil {
		return err
	}
	req.Header = p.request.Header
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	h := fmt.Sprintf("%x", md5.Sum(b))

	if resp.StatusCode == 200 {
		if p.lastHash[idx] == h {
			log.Infof("Response unchanged")
			return nil
		}
		p.lastHash[idx] = h
	}

	hn, _ := os.Hostname()
	prr := &PollResponse{
		FromURL:      url,
		Hash:         h,
		Status:       resp.Status,
		StatusCode:   resp.StatusCode,
		Header:       resp.Header,
		Body:         string(b),
		PollVersion:  Version(),
		PollHostname: hn,
	}

	log.Infof("Got response %v: %v, sending %d bytes to server", resp.Status, h, len(prr.Body))

	// Write message back to server as flate-compressed JSON
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, 3)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(fw).Encode(prr); err != nil {
		return err
	}
	if err := fw.Close(); err != nil {
		return err
	}
	return p.conn.WriteMessage(websocket.BinaryMessage, buf.Bytes())
}

func delay(ctx context.Context, d time.Duration) {
	if ctx.Err() != nil {
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func (p *Poller) RunOnce(ctx context.Context) error {
	log.Infof("Connecting to %s", *server)
	var err error
	p.conn, _, err = websocket.DefaultDialer.DialContext(ctx, *server, nil)
	if err != nil {
		return err
	}
	defer p.conn.Close()

	log.Infof("Connected")

	requestc := make(chan *PollRequest, 1)
	errc := make(chan error, 1)

	go func() {
		defer close(requestc)
		defer close(errc)
		for ctx.Err() == nil {
			_, r, err := p.conn.NextReader()
			if err != nil {
				errc <- err
				return
			}
			req := &PollRequest{}
			if err := json.NewDecoder(r).Decode(req); err != nil {
				log.Errorf("Failed to decode incoming command %v", err)
				continue
			}
			requestc <- req
		}
	}()

	for ctx.Err() == nil {
		var nextc <-chan time.Time
		if p.request != nil {
			if err := p.DoPoll(ctx); err != nil {
				log.Errorf("Poll failed: %v", err)
				delay(ctx, time.Second)
				continue
			}
			jt := p.request.Jitter / time.Duration(float64(1)/(rng.Float64()-float64(0.5)))
			ival := p.request.Interval + jt
			log.Infof("Waiting for %v until next iteration", ival)
			nextc = time.After(ival)
		} else {
			log.Infof("Waiting for initial command")
		}
		select {
		case req := <-requestc:
			if req == nil {
				continue
			}
			log.Infof("Poll target changed.")
			p.request = req
			p.idx = 0
			p.lastHash = make([]string, len(p.request.URLs))
		case err := <-errc:
			return err
		case <-ctx.Done():
		case <-nextc:
		}
	}

	return nil
}

func ServeVersion(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Version: %s", Version())
}

func main() {
	flag.Parse()

	// Configure logging.
	customFormatter := new(log.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	customFormatter.FullTimestamp = true
	log.SetFormatter(customFormatter)

	log.Infof("Now running %s", Version())

	ctx := topLevelContext()

	http.HandleFunc("/", ServeVersion)
	go http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)

	for ctx.Err() == nil {
		p := &Poller{}
		if err := p.RunOnce(ctx); err != nil {
			log.Errorf("runOnce %v", err)
			delay(ctx, 10*time.Second)
		}
	}
}
