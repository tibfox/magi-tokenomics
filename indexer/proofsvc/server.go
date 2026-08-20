package proofsvc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Source supplies the rows this service rebuilds from. It is an interface so the
// tests can drive the whole service off fixtures without a database, and so an
// operator can point it at something other than Hasura.
type Source interface {
	Root(channel string, epoch int64) (RootRow, error)
	Pages(channel string, epoch int64) ([]SharesRow, error)
}

// Server serves proofs for finalized epochs.
type Server struct {
	src Source

	// Rebuilding is O(book) and every claimant in an epoch asks for the same
	// tree, so a finalized epoch is built once. Only SUCCESSFUL builds are
	// cached: caching a failure would freeze a transient indexer lag into a
	// permanent refusal, and the failures this service reports are exactly the
	// ones an operator fixes by letting the indexer catch up.
	mu    sync.Mutex
	cache map[string]*Book
}

func New(src Source) *Server {
	return &Server{src: src, cache: map[string]*Book{}}
}

func (s *Server) book(channel string, epoch int64) (*Book, error) {
	key := channel + "|" + strconv.FormatInt(epoch, 10)
	s.mu.Lock()
	b, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	root, err := s.src.Root(channel, epoch)
	if err != nil {
		return nil, err
	}
	pages, err := s.src.Pages(channel, epoch)
	if err != nil {
		return nil, err
	}
	b, err = BuildBook(root, pages)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[key] = b
	s.mu.Unlock()
	return b, nil
}

type errBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// params pulls the channel/epoch every endpoint takes.
func params(r *http.Request) (string, int64, error) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		return "", 0, fmt.Errorf("channel is required")
	}
	epStr := r.URL.Query().Get("epoch")
	if epStr == "" {
		return "", 0, fmt.Errorf("epoch is required")
	}
	// Parsed as a NUMBER and used as one. The contract treats "00" as a distinct
	// epoch string from "0" and refuses it; accepting the alias here would hand
	// out proofs under a spelling no claim can use.
	ep, err := strconv.ParseInt(epStr, 10, 64)
	if err != nil || ep < 0 || epStr != strconv.FormatInt(ep, 10) {
		return "", 0, fmt.Errorf("epoch must be a canonical non-negative integer, not %q", epStr)
	}
	return ch, ep, nil
}

// handleProof answers "what can this account claim, and how does it prove it".
func (s *Server) handleProof(w http.ResponseWriter, r *http.Request) {
	ch, ep, err := params(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	acct := r.URL.Query().Get("account")
	if acct == "" {
		writeJSON(w, http.StatusBadRequest, errBody{"account is required"})
		return
	}
	b, err := s.book(ch, ep)
	if err != nil {
		// 503, not 500: every build failure here is "the indexer's copy is not
		// usable YET or is wrong", and a claimant retrying later is the correct
		// response to all of them.
		writeJSON(w, http.StatusServiceUnavailable, errBody{err.Error()})
		return
	}
	share, path, err := b.Proof(acct)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel": ch, "epoch": ep, "account": acct,
		"share": share, "proof": path, "root": b.Root,
		"total_shares": b.Total.String(),
		"claim_payload": fmt.Sprintf(`{"channel":"%s","epoch":"%d","share":"%s","proof":"%s"}`,
			ch, ep, share, joinProof(path)),
	})
}

// handleRoot reports what this service rebuilt, so an operator can compare it
// against the chain without asking for anybody's proof.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	ch, ep, err := params(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	b, err := s.book(ch, ep)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody{err.Error()})
		return
	}
	skipped := []map[string]string{}
	for _, sk := range b.Skipped {
		skipped = append(skipped, map[string]string{"entry": sk.Raw, "reason": sk.Reason})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel": ch, "epoch": ep, "root": b.Root,
		"total_shares": b.Total.String(), "accounts": len(b.Accounts()),
		// Surfaced, not hidden. A skipped entry is a no-op inside a transaction
		// that SUCCEEDED, so nothing else records that the earner was ever there.
		"skipped": skipped,
	})
}

// handleHealth is deliberately dumb: it reports that the process is up, and
// nothing about whether any epoch is servable. Tying liveness to a book build
// would take the service down whenever the indexer lags, which is the moment
// operators most need it answering.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func joinProof(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

// Handler returns the service's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/proof", s.handleProof)
	mux.HandleFunc("/root", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	return mux
}

// Serve runs the service until the process ends.
func (s *Server) Serve(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("proofsvc listening on %s", addr)
	return srv.ListenAndServe()
}
