package vmcontext

import (
	"context"
	"fmt"
	"github.com/filecoin-project/go-state-types/exitcode"
	specsruntime "github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/ipfs/go-cid"
	"reflect"

	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/venus/pkg/vm/gas"
	"github.com/filecoin-project/venus/pkg/vm/runtime"
	"github.com/filecoin-project/venus/pkg/vm/storage"
)

type vmStorage interface {
	GetWithLen(ctx context.Context, cid cid.Cid, obj interface{}) (int, error)
	PutWithLen(ctx context.Context, obj interface{}) (cid.Cid, int, error)
}

// ActorStorage hides the storage methods From the actors and turns the errors into runtime panics.
type ActorStorage struct {
	context   context.Context
	inner     vmStorage
	pricelist gas.Pricelist
	gasTank   *gas.GasTracker
}

func NewActorStorage(ctx context.Context, inner vmStorage, gasTank *gas.GasTracker, pricelist gas.Pricelist) *ActorStorage {
	return &ActorStorage{
		context:   ctx,
		inner:     inner,
		pricelist: pricelist,
		gasTank:   gasTank,
	}
}

//
// implement runtime.Store for ActorStorage
//

var _ specsruntime.Store = (*ActorStorage)(nil)

// Serialization technically belongs in the actor code, rather than inside the VM.
// The true VM storage interface is in terms of raw bytes and, when we have user-defined,
// serialization code will be directly in those contracts.
// Our present runtime interface is at a slightly higher level for convenience, but the exit code here is the
// actor, rather than system-level, error code.
const serializationErr = exitcode.ErrSerialization

func (s *ActorStorage) StorePut(obj cbor.Marshaler) cid.Cid {
	cid, ln, err := s.inner.PutWithLen(s.context, obj)
	if err != nil {
		msg := fmt.Sprintf("failed To put object %s in store: %s", reflect.TypeOf(obj), err)
		if _, ok := err.(storage.SerializationError); ok {
			runtime.Abortf(serializationErr, msg)
		} else {
			panic(msg)
		}
	}
	s.gasTank.Charge(s.pricelist.OnIpldPut(ln), "storage put %s %d bytes into %v", cid, ln, obj)
	return cid
}

func (s *ActorStorage) StoreGet(cid cid.Cid, obj cbor.Unmarshaler) bool {
	//gas charge must check first
	s.gasTank.Charge(s.pricelist.OnIpldGet(), "storage get %s bytes into %v", cid, obj)
	_, err := s.inner.GetWithLen(s.context, cid, obj)
	if err == storage.ErrNotFound {
		return false
	}
	if err != nil {
		msg := fmt.Sprintf("failed To get object %s %s From store: %s", reflect.TypeOf(obj), cid, err)
		if _, ok := err.(storage.SerializationError); ok {
			runtime.Abortf(serializationErr, msg)
		} else {
			panic(msg)
		}
	}
	return true
}
