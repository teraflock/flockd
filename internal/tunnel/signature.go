package tunnel

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	"google.golang.org/protobuf/proto"
)

// ErrBadSignature means a DispatchRequest failed verification against the
// pinned coordinator key. Such dispatches are never executed (SPEC §6).
var ErrBadSignature = errors.New("tunnel: dispatch signature verification failed")

// dispatchSigningBytes returns the canonical serialization of a dispatch
// with the signature field cleared (the byte string the coordinator signs).
func dispatchSigningBytes(d *tunnelv1.DispatchRequest) ([]byte, error) {
	clone, ok := proto.Clone(d).(*tunnelv1.DispatchRequest)
	if !ok {
		return nil, fmt.Errorf("tunnel: clone dispatch")
	}
	clone.Signature = nil
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return nil, fmt.Errorf("tunnel: marshal dispatch for signing: %w", err)
	}
	return raw, nil
}

// SignDispatch sets d.Signature (coordinator side; used by fakecoord and
// control-plane tests).
func SignDispatch(priv ed25519.PrivateKey, d *tunnelv1.DispatchRequest) error {
	raw, err := dispatchSigningBytes(d)
	if err != nil {
		return err
	}
	d.Signature = ed25519.Sign(priv, raw)
	return nil
}

// VerifyDispatch checks d.Signature against the pinned coordinator key.
func VerifyDispatch(pub ed25519.PublicKey, d *tunnelv1.DispatchRequest) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("tunnel: invalid coordinator pubkey length %d", len(pub))
	}
	raw, err := dispatchSigningBytes(d)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, raw, d.GetSignature()) {
		return ErrBadSignature
	}
	return nil
}
