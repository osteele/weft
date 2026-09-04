package edge

import (
	"bytes"
	"encoding/base64"
	"fmt"
)

// FrameMarker is the first line of every submission object. It guards the
// framing only. The schema is guarded separately by the envelope's
// ProtocolVersion, which cannot be read until the signature has verified.
const FrameMarker = "weft-edge/1"

// frame is a submission object split into its parts. Holding a frame conveys
// nothing about authenticity; only Verify does that.
type frame struct {
	keyID     string
	signature []byte
	// envelope holds the exact octets the signature covers. They are never
	// re-serialized, which is why no canonical encoding is needed.
	envelope []byte
}

// encodeFrame builds the wire object. Only the signer calls this.
func encodeFrame(keyID string, signature, envelope []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(FrameMarker)
	buf.WriteByte('\n')
	buf.WriteString(keyID)
	buf.WriteByte('\n')
	buf.WriteString(base64.StdEncoding.EncodeToString(signature))
	buf.WriteByte('\n')
	buf.Write(envelope)
	return buf.Bytes()
}

// parseFrame splits the object into marker, key id, signature, and envelope
// octets. It performs no cryptography and returns no envelope fields; the
// returned envelope bytes are untrusted until Verify has checked them.
func parseFrame(object []byte) (*frame, *Refusal) {
	marker, rest, ok := bytes.Cut(object, []byte{'\n'})
	if !ok {
		return nil, refuse(ReasonUnframed, "object has no frame marker line")
	}
	if string(marker) != FrameMarker {
		return nil, refuse(ReasonUnframed,
			"unrecognized frame marker %q (expected %q)", truncate(marker), FrameMarker)
	}
	keyID, rest, ok := bytes.Cut(rest, []byte{'\n'})
	if !ok {
		return nil, refuse(ReasonUnframed, "object ends before the key id line")
	}
	if len(keyID) == 0 {
		return nil, refuse(ReasonUnframed, "key id line is empty")
	}
	sigLine, envelope, ok := bytes.Cut(rest, []byte{'\n'})
	if !ok {
		return nil, refuse(ReasonUnframed, "object ends before the signature line")
	}
	signature, err := base64.StdEncoding.DecodeString(string(sigLine))
	if err != nil {
		return nil, refuse(ReasonUnframed, "signature line is not valid base64: %v", err)
	}
	if len(envelope) == 0 {
		return nil, refuse(ReasonUnframed, "object carries no envelope")
	}
	return &frame{keyID: string(keyID), signature: signature, envelope: envelope}, nil
}

func truncate(b []byte) string {
	const max = 32
	if len(b) > max {
		return fmt.Sprintf("%s...", b[:max])
	}
	return string(b)
}
