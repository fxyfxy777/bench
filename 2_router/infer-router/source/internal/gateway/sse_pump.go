package gateway

import (
	"io"
	"net/http"
)

// sseObserver is invoked for each non-empty chunk read from the upstream body,
// just before the chunk is written to the client. Used by streamForward to
// record TTFT, detect the SSE [DONE] marker, and feed a streamCapture for
// usage / finish_reason extraction.
//
// Observers MUST NOT retain the chunk slice — it is owned by the pool buffer
// and reused on the next iteration.
type sseObserver func(chunk []byte)

// pumpSSE reads from upstream and forwards chunks to the client, flushing
// after each non-empty write. It is the single shared SSE transfer loop used
// by both the normal V2 stream path (streamForward) and the PD splitwise path.
//
// Returns:
//   - errClientDisconnect if w.Write fails (caller is expected to cancel the
//     backend context to stop GPU inference).
//   - The wrapped read error on upstream body failure other than io.EOF.
//   - nil on clean io.EOF.
//
// The buffer is sourced from v2StreamBufPool to avoid per-request allocation.
func pumpSSE(w http.ResponseWriter, body io.Reader, observer sseObserver) error {
	buf := v2StreamBufPool.Get().([]byte)
	defer v2StreamBufPool.Put(buf) //nolint:staticcheck

	flusher, _ := w.(http.Flusher)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if observer != nil {
				observer(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return errClientDisconnect
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}
