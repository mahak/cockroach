// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver_test

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/cloud/nodelocal"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/config/zonepb"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/keyvisualizer"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/abortspan"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/batcheval"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/gc"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverbase"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/replicastats"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/stateloader"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc/rpcbase"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/spanconfig/spanconfigtestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/bootstrap"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/systemschema"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/fs"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/storageutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/ts"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/upgrade/upgradebase"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/log/logpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/cockroach/pkg/util/tracing/tracingpb"
	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"
)

// adminSplitArgs creates an AdminSplitRequest for the provided split key.
func adminSplitArgs(splitKey roachpb.Key) *kvpb.AdminSplitRequest {
	return &kvpb.AdminSplitRequest{
		RequestHeader: kvpb.RequestHeader{
			Key: splitKey,
		},
		SplitKey: splitKey,
	}
}

// TestStoreRangeSplitAtIllegalKeys verifies a range cannot be split
// at illegal keys.
func TestStoreRangeSplitAtIllegalKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	for _, key := range []roachpb.Key{
		keys.Meta1Prefix,
		testutils.MakeKey(keys.Meta1Prefix, []byte("a")),
		testutils.MakeKey(keys.Meta1Prefix, roachpb.RKeyMax),
		keys.Meta2KeyMax,
		testutils.MakeKey(keys.Meta2KeyMax, []byte("a")),
	} {
		args := adminSplitArgs(key)
		_, pErr := kv.SendWrapped(context.Background(), s.DB().NonTransactionalSender(), args)
		if !testutils.IsPError(pErr, "cannot split") {
			t.Errorf("%q: unexpected split error %s", key, pErr)
		}
	}
}

// Verify that on a split, only the non-expired abort span records are copied
// into the right hand side of the split.
func TestStoreSplitAbortSpan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	left, middle, right := roachpb.Key("a"), roachpb.Key("b"), roachpb.Key("c")

	txn := func(key roachpb.Key, ts hlc.Timestamp) *roachpb.Transaction {
		txn := roachpb.MakeTransaction("test", key, 0, 0, ts, 0, int32(s.SQLInstanceID()), 0, false /* omitInRangefeeds */)
		return &txn
	}

	var expAll []roachpb.AbortSpanEntry

	populateAbortSpan := func(key roachpb.Key, ts hlc.Timestamp) *kvpb.ResolveIntentRequest {
		pushee := txn(key, ts)

		// First write an intent on the key...
		incArgs := incrementArgs(key, 1)
		_, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{Txn: pushee}, incArgs)
		if pErr != nil {
			t.Fatalf("while sending +%v: %s", incArgs, pErr)
		}

		// Then resolve the intent and poison. Without the intent write, the
		// intent resolution would be a no-op and wouldn't leave an AbortSpan
		// entry.
		expAll = append(expAll, roachpb.AbortSpanEntry{
			Key:       key,
			Timestamp: ts,
		})
		return &kvpb.ResolveIntentRequest{
			RequestHeader: kvpb.RequestHeader{
				Key: key,
			},
			IntentTxn: pushee.TxnMeta,
			Status:    roachpb.ABORTED,
			Poison:    true,
		}
	}

	key := func(k roachpb.Key, i int) roachpb.Key {
		var r []byte
		r = append(r, k...)
		r = append(r, []byte(strconv.Itoa(i))...)
		return r
	}

	thresh := gc.TxnCleanupThreshold.Default().Nanoseconds()
	// Make sure this test doesn't run out of padding time if we significantly
	// reduce TxnCleanupThreshold in the future for whatever reason.
	require.Greater(t, thresh, int64(time.Minute))
	// Pick a non-gcable and gcable timestamp, respectively.
	// Use the current time for some non-expired abort span records.
	// Note that the cleanup threshold is so large that while this test runs,
	// these records won't expire.
	tsFresh := hlc.Timestamp{WallTime: s.Clock().Now().WallTime}
	tsStale := hlc.Timestamp{WallTime: s.Clock().Now().WallTime - thresh - 1}

	args := []kvpb.Request{
		populateAbortSpan(key(left, 1), tsFresh),
		populateAbortSpan(key(left, 2), tsStale),
		populateAbortSpan(key(middle, 1), tsFresh),
		populateAbortSpan(key(middle, 2), tsStale),
		populateAbortSpan(key(right, 1), tsFresh),
		populateAbortSpan(key(right, 2), tsStale),
		adminSplitArgs(middle),
	}

	// Nothing gets removed from the LHS during the split. This could
	// be done but has to be done carefully to avoid large Raft proposals,
	// and the stats computation needs to be checked carefully.
	expL := []roachpb.AbortSpanEntry{
		{Key: key(left, 1), Timestamp: tsFresh},
		{Key: key(left, 2), Timestamp: tsStale},
		{Key: key(middle, 1), Timestamp: tsFresh},
		{Key: key(middle, 2), Timestamp: tsStale},
		{Key: key(right, 1), Timestamp: tsFresh},
		{Key: key(right, 2), Timestamp: tsStale},
	}

	// But we don't blindly copy everything over to the RHS. Only entries with
	// recent timestamp are duplicated. This is important because otherwise the
	// Raft command size can blow up and splits fail.
	expR := []roachpb.AbortSpanEntry{
		{Key: key(left, 1), Timestamp: tsFresh},
		{Key: key(middle, 1), Timestamp: tsFresh},
		{Key: key(right, 1), Timestamp: tsFresh},
	}

	for _, arg := range args {
		_, pErr := kv.SendWrapped(ctx, store.TestSender(), arg)
		if pErr != nil {
			t.Fatalf("while sending +%v: %s", arg, pErr)
		}
	}

	collect := func(as *abortspan.AbortSpan) []roachpb.AbortSpanEntry {
		var results []roachpb.AbortSpanEntry
		if err := as.Iterate(ctx, store.TODOEngine(), func(_ roachpb.Key, entry roachpb.AbortSpanEntry) error {
			entry.Priority = 0 // don't care about that
			results = append(results, entry)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Slice(results, func(i, j int) bool {
			c := bytes.Compare(results[i].Key, results[j].Key)
			if c == 0 {
				return results[i].Timestamp.Less(results[j].Timestamp)
			}
			return c < 0
		})
		return results
	}

	l := collect(store.LookupReplica(keys.MustAddr(left)).AbortSpan())
	r := collect(store.LookupReplica(keys.MustAddr(right)).AbortSpan())

	if !reflect.DeepEqual(expL, l) {
		t.Fatalf("left hand side: expected %+v, got %+v", expL, l)
	}
	if !reflect.DeepEqual(expR, r) {
		t.Fatalf("right hand side: expected %+v, got %+v", expR, r)
	}
}

// TestStoreRangeSplitInsideRow verifies an attempt to split a range inside of
// a table row will cause a split at a boundary between rows.
func TestStoreRangeSplitInsideRow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Manually create some the column keys corresponding to the table:
	//
	//   CREATE TABLE t (id STRING PRIMARY KEY, col1 INT, col2 INT)
	tableKey := roachpb.RKey(keys.SystemSQLCodec.TablePrefix(bootstrap.TestingUserDescID(0)))
	rowKey := roachpb.Key(encoding.EncodeVarintAscending(append([]byte(nil), tableKey...), 1))
	rowKey = encoding.EncodeStringAscending(encoding.EncodeVarintAscending(rowKey, 1), "a")
	col1Key, err := keys.EnsureSafeSplitKey(keys.MakeFamilyKey(append([]byte(nil), rowKey...), 1))
	if err != nil {
		t.Fatal(err)
	}
	col2Key, err := keys.EnsureSafeSplitKey(keys.MakeFamilyKey(append([]byte(nil), rowKey...), 2))
	if err != nil {
		t.Fatal(err)
	}

	// We don't care about the value, so just store any old thing.
	if err := store.DB().Put(ctx, col1Key, "column 1"); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Put(ctx, col2Key, "column 2"); err != nil {
		t.Fatal(err)
	}

	// Split between col1Key and col2Key by splitting before col2Key.
	args := adminSplitArgs(col2Key)
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), args)
	if pErr != nil {
		t.Fatalf("%s: split unexpected error: %s", col1Key, pErr)
	}

	repl1 := store.LookupReplica(roachpb.RKey(col1Key))
	repl2 := store.LookupReplica(roachpb.RKey(col2Key))

	// Verify the two columns are still on the same range.
	if !reflect.DeepEqual(repl1, repl2) {
		t.Fatalf("%s: ranges differ: %+v vs %+v", col1Key, repl1, repl2)
	}
	// Verify we split on a row key.
	if startKey := repl1.Desc().StartKey; !startKey.Equal(rowKey) {
		t.Fatalf("%s: expected split on %s, but found %s", col1Key, rowKey, startKey)
	}

	// Verify the previous range was split on a row key.
	repl3 := store.LookupReplica(tableKey)
	if endKey := repl3.Desc().EndKey; !endKey.Equal(rowKey) {
		t.Fatalf("%s: expected split on %s, but found %s", col1Key, rowKey, endKey)
	}
}

// TestStoreRangeSplitIntents executes a split of a range and verifies
// that all intents are cleared and the transaction record cleaned up.
func TestStoreRangeSplitIntents(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// First, write some values left and right of the proposed split key.
	pArgs := putArgs([]byte("c"), []byte("foo"))
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs([]byte("x"), []byte("bar"))
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Split the range.
	splitKey := roachpb.Key("m")
	args := adminSplitArgs(splitKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	// Verify no intents remains on range descriptor keys.
	splitKeyAddr, err := keys.Addr(splitKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []roachpb.Key{keys.RangeDescriptorKey(roachpb.RKeyMin), keys.RangeDescriptorKey(splitKeyAddr)} {
		if _, err := storage.MVCCGet(
			ctx, store.TODOEngine(), key, store.Clock().Now(), storage.MVCCGetOptions{},
		); err != nil {
			t.Errorf("failed to read consistent range descriptor for key %s: %+v", key, err)
		}
	}

	txnPrefix := func(key roachpb.Key) roachpb.Key {
		rk, err := keys.Addr(key)
		if err != nil {
			t.Fatal(err)
		}
		return keys.MakeRangeKey(rk, keys.LocalTransactionSuffix, nil)
	}
	// Verify the transaction record is gone.
	start := storage.MakeMVCCMetadataKey(keys.MakeRangeKeyPrefix(roachpb.RKeyMin))
	end := storage.MakeMVCCMetadataKey(keys.MakeRangeKeyPrefix(roachpb.RKeyMax))
	iter, err := store.TODOEngine().NewMVCCIterator(context.Background(), storage.MVCCKeyAndIntentsIterKind, storage.IterOptions{UpperBound: end.Key})
	if err != nil {
		t.Fatal(err)
	}

	defer iter.Close()
	for iter.SeekGE(start); ; iter.Next() {
		if ok, err := iter.Valid(); err != nil {
			t.Fatal(err)
		} else if !ok {
			break
		}

		if bytes.HasPrefix([]byte(iter.UnsafeKey().Key), txnPrefix(roachpb.KeyMin)) ||
			bytes.HasPrefix([]byte(iter.UnsafeKey().Key), txnPrefix(splitKey)) {
			t.Errorf("unexpected system key: %s; txn record should have been cleaned up", iter.UnsafeKey())
		}
	}
}

// TestQueryLocksAcrossRanges attempts to split to create multiple ranges,
// create transactions that hold locks on keys across the ranges, and then
// utilize the QueryLocks API to validate the exposed lock table information.
func TestQueryLocksAcrossRanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// First, write some values left and right of the proposed split key.
	pArgs := putArgs([]byte("c"), []byte("foo"))
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs([]byte("p"), []byte("fizzbuzz"))
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs([]byte("x"), []byte("bar"))
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Split the range.
	splitKey := roachpb.Key("m")
	args := adminSplitArgs(splitKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	rng1 := store.LookupReplica(roachpb.RKey("c")).Desc()
	rng2 := store.LookupReplica(roachpb.RKey("p")).Desc()

	// Use txn1 to get a replicated lock on "c" and an unreplicated lock on "x".
	txn1 := kv.NewTxn(ctx, db, s.NodeID())
	err = txn1.Put(ctx, roachpb.Key("c"), []byte("baz"))
	require.NoError(t, err)
	_, err = txn1.GetForUpdate(ctx, roachpb.Key("x"), kvpb.BestEffort)
	require.NoError(t, err)

	// Use txn2 to get an unreplicated lock on "p".
	txn2 := kv.NewTxn(ctx, db, s.NodeID())
	_, err = txn2.GetForUpdate(ctx, roachpb.Key("p"), kvpb.BestEffort)
	require.NoError(t, err)

	now := s.Clock().NowAsClockTimestamp()
	txn3Proto := roachpb.MakeTransaction(
		"waiter",
		nil, // baseKey
		isolation.Serializable,
		roachpb.NormalUserPriority,
		s.Clock().NowAsClockTimestamp().ToTimestamp(),
		s.Clock().MaxOffset().Nanoseconds(),
		int32(s.SQLInstanceID()),
		0,
		false, // omitInRangefeeds
	)
	txn3ID := txn3Proto.ID

	// Execute a scan request that includes the locked keys, such that this
	// operation will be forced to wait on txn1 and txn2.
	scanRes := make(chan error)
	go func() {
		txn3 := kv.NewTxnFromProto(ctx, db, s.NodeID(), now, kv.RootTxn, &txn3Proto)
		_, err = txn3.Scan(ctx, roachpb.Key("a"), roachpb.Key("z"), 0)
		scanRes <- err
	}()

	select {
	case err := <-scanRes:
		t.Fatalf("scan did not wait on locks, returned err %v", err)
	case <-time.After(100 * time.Millisecond):
		// Before the scan can return, attempt to query locks.
		// TODO(sarkesian): install a new testing knob that we can used fire an
		// event when the lock wait begins, rather than relying on a timeout.
	}

	// Issue QueryLocksRequest and validate results. We should see 2 locks,
	// one replicated lock "c" held by txn1 and one unreplicated lock on "p"
	// held by txn2, both with txn3 currently waiting to read.
	b := kv.Batch{}
	b.Header.MaxSpanRequestKeys = 2
	queryLocksRequest := &kvpb.QueryLocksRequest{
		RequestHeader: kvpb.RequestHeader{
			Key:    roachpb.Key("a"),
			EndKey: roachpb.Key("z"),
		},
		IncludeUncontended: true,
	}
	b.AddRawRequest(queryLocksRequest)
	err = db.Run(ctx, &b)
	require.NoError(t, err)

	res := b.RawResponse().Responses[0].GetQueryLocks()
	require.Len(t, res.Locks, 2)
	require.Equal(t, kvpb.RESUME_KEY_LIMIT, res.ResumeReason,
		"observed locks on %v and %v", res.Locks[0].Key, res.Locks[1].Key,
	)
	require.Equal(t, roachpb.Key("x"), res.ResumeSpan.Key)
	require.Equal(t, roachpb.Key("z"), res.ResumeSpan.EndKey)

	require.Equal(t, roachpb.Key("c"), res.Locks[0].Key)
	require.NotNil(t, res.Locks[0].LockHolder)
	require.Equal(t, txn1.ID(), res.Locks[0].LockHolder.ID)
	require.Equal(t, rng1.RangeID, res.Locks[0].RangeID)
	require.Equal(t, lock.Replicated, res.Locks[0].Durability)
	require.Len(t, res.Locks[0].Waiters, 1)
	require.Equal(t, txn3ID, res.Locks[0].Waiters[0].WaitingTxn.ID)

	require.Equal(t, roachpb.Key("p"), res.Locks[1].Key)
	require.NotNil(t, res.Locks[1].LockHolder)
	require.Equal(t, txn2.ID(), res.Locks[1].LockHolder.ID)
	require.Equal(t, rng2.RangeID, res.Locks[1].RangeID)
	require.Equal(t, lock.Unreplicated, res.Locks[1].Durability)
	require.Len(t, res.Locks[1].Waiters, 1)
	require.Equal(t, txn3ID, res.Locks[1].Waiters[0].WaitingTxn.ID)

	// Cleanup remaining transactions holding locks and unblock scan.
	require.NoError(t, txn1.Commit(ctx))
	require.NoError(t, txn2.Commit(ctx))

	// The scan should finish once txn1 and txn2 commit and release locks.
	err = <-scanRes
	require.NoError(t, err, "txn3.Scan() should have finished without error after txns unlock")
}

// TestStoreRangeSplitAtRangeBounds verifies that attempting to
// split a range at its start key is a no-op and does not actually
// perform a split (would create zero-length range!). This sort
// of thing might happen in the wild if two split requests arrived for
// same key. The first one succeeds and second would try to split
// at the start of the newly split range.
func TestStoreRangeSplitAtRangeBounds(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Split range 1 at an arbitrary key.
	key := roachpb.Key("a")
	rngID := store.LookupReplica(roachpb.RKey(key)).RangeID
	h := kvpb.Header{RangeID: rngID}
	args := adminSplitArgs(key)
	if _, pErr := kv.SendWrappedWith(ctx, store, h, args); pErr != nil {
		t.Fatal(pErr)
	}
	replCount := store.ReplicaCount()

	// An AdminSplit request sent to the end of the old range should be re-routed
	// to the start of the new range, succeeding but without creating any new ranges.
	if _, pErr := kv.SendWrappedWith(ctx, store, h, args); pErr != nil {
		t.Fatal(pErr)
	}

	postEndSplitReplCount := store.ReplicaCount()
	if replCount != postEndSplitReplCount {
		t.Fatalf("splitting at the end of a range should not create a new range; before second split "+
			"found %d ranges, after second split found %d ranges", replCount, postEndSplitReplCount)
	}

	// An AdminSplit request sent to the start of the new range
	// should succeed but no new ranges should be created.
	newRng := store.LookupReplica(roachpb.RKey(key))
	h.RangeID = newRng.RangeID
	if _, pErr := kv.SendWrappedWith(ctx, store, h, args); pErr != nil {
		t.Fatal(pErr)
	}

	postStartSplitReplCount := store.ReplicaCount()
	if postEndSplitReplCount != postStartSplitReplCount {
		t.Fatalf("splitting at the start boundary should not create a new range; before third split "+
			"found %d ranges, after fourth split found %d ranges", postEndSplitReplCount, postEndSplitReplCount)
	}
}

// TestStoreRangeSplitIdempotency executes a split of a range and
// verifies that the resulting ranges respond to the right key ranges
// and that their stats have been properly accounted for and requests
// can't be replayed.
func TestStoreRangeSplitIdempotency(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:              true,
				DisableSplitQueue:              true,
				DisableCanAckBeforeApplication: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	splitKey := roachpb.Key("m")
	content := roachpb.Key("asdvb")

	// First, write some values left and right of the proposed split key.
	pArgs := putArgs([]byte("c"), content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs([]byte("x"), content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Increments are a good way of testing idempotency. Up here, we
	// address them to the original range, then later to the one that
	// contains the key.
	txn := roachpb.MakeTransaction("test", []byte("c"), isolation.Serializable, 10, store.Clock().Now(), 0, int32(s.SQLInstanceID()), 0, false /* omitInRangefeeds */)
	lIncArgs := incrementArgs([]byte("apoptosis"), 100)
	lTxn := txn
	lTxn.Sequence++
	lIncArgs.Sequence = lTxn.Sequence
	if _, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
		Txn: &lTxn,
	}, lIncArgs); pErr != nil {
		t.Fatal(pErr)
	}
	rIncArgs := incrementArgs([]byte("wobble"), 10)
	rTxn := txn
	rTxn.Sequence++
	rIncArgs.Sequence = rTxn.Sequence
	if _, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
		Txn: &rTxn,
	}, rIncArgs); pErr != nil {
		t.Fatal(pErr)
	}

	originalRepl := store.LookupReplica(roachpb.RKey(splitKey))
	require.NotNil(t, originalRepl)
	// Get the original stats for key and value bytes.
	ms, err := stateloader.Make(originalRepl.RangeID).LoadMVCCStats(ctx, store.TODOEngine())
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, valBytes := ms.KeyBytes, ms.ValBytes

	// Split the range.
	args := adminSplitArgs(splitKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	// Verify no intents remains on range descriptor keys.
	splitKeyAddr, err := keys.Addr(splitKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []roachpb.Key{keys.RangeDescriptorKey(roachpb.RKeyMin), keys.RangeDescriptorKey(splitKeyAddr)} {
		if _, err := storage.MVCCGet(
			context.Background(), store.TODOEngine(), key, store.Clock().Now(), storage.MVCCGetOptions{},
		); err != nil {
			t.Fatal(err)
		}
	}

	rngDesc := originalRepl.Desc()
	newRng := store.LookupReplica([]byte("m"))
	newRngDesc := newRng.Desc()
	if !bytes.Equal(newRngDesc.StartKey, splitKey) || !bytes.Equal(splitKey, rngDesc.EndKey) {
		t.Errorf("ranges mismatched, wanted %q=%q=%q", newRngDesc.StartKey, splitKey, rngDesc.EndKey)
	}

	// Try to get values from both left and right of where the split happened.
	gArgs := getArgs([]byte("c"))
	if reply, pErr := kv.SendWrapped(ctx, store.TestSender(), gArgs); pErr != nil {
		t.Fatal(pErr)
	} else if replyBytes, pErr := reply.(*kvpb.GetResponse).Value.GetBytes(); pErr != nil {
		t.Fatal(pErr)
	} else if !bytes.Equal(replyBytes, content) {
		t.Fatalf("actual value %q did not match expected value %q", replyBytes, content)
	}
	gArgs = getArgs([]byte("x"))
	if reply, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
		RangeID: newRng.RangeID,
	}, gArgs); pErr != nil {
		t.Fatal(pErr)
	} else if replyBytes, err := reply.(*kvpb.GetResponse).Value.GetBytes(); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(replyBytes, content) {
		t.Fatalf("actual value %q did not match expected value %q", replyBytes, content)
	}

	// Send out an increment request copied from above (same txn/sequence)
	// which remains in the old range.
	_, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
		Txn: &lTxn,
	}, lIncArgs)
	if pErr != nil {
		t.Fatal(pErr)
	}

	// Send out the same increment copied from above (same txn/sequence), but
	// now to the newly created range (which should hold that key).
	_, pErr = kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
		RangeID: newRng.RangeID,
		Txn:     &rTxn,
	}, rIncArgs)
	if pErr != nil {
		t.Fatal(pErr)
	}

	// Compare stats of split ranges to ensure they are non zero and
	// exceed the original range when summed.
	left, err := stateloader.Make(originalRepl.RangeID).LoadMVCCStats(ctx, store.TODOEngine())
	if err != nil {
		t.Fatal(err)
	}
	lKeyBytes, lValBytes := left.KeyBytes, left.ValBytes
	right, err := stateloader.Make(newRng.RangeID).LoadMVCCStats(ctx, store.TODOEngine())
	if err != nil {
		t.Fatal(err)
	}
	rKeyBytes, rValBytes := right.KeyBytes, right.ValBytes

	if lKeyBytes == 0 || rKeyBytes == 0 {
		t.Errorf("expected non-zero key bytes; got %d, %d", lKeyBytes, rKeyBytes)
	}
	if lValBytes == 0 || rValBytes == 0 {
		t.Errorf("expected non-zero val bytes; got %d, %d", lValBytes, rValBytes)
	}
	if lKeyBytes+rKeyBytes != keyBytes {
		t.Errorf("left + right key bytes don't match; %d + %d <= %d", lKeyBytes, rKeyBytes, keyBytes)
	}
	if lValBytes+rValBytes != valBytes {
		t.Errorf("left + right val bytes don't match; %d + %d <= %d", lValBytes, rValBytes, valBytes)
	}
}

// TestStoreRangeSplitMergeStats starts by splitting the system keys from
// user-space keys and verifying that the user space side of the split (which is
// empty), has all zeros for stats. It then writes random data to the user space
// side, splits it halfway and verifies the two splits have appropriate stats.
// Finally, it merges the ranges back and asserts that the stats equal the
// original stats.
func TestStoreRangeSplitMergeStats(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:              true,
				DisableSplitQueue:              true,
				DisableCanAckBeforeApplication: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Split the range after the last table data key.
	keyPrefix := keys.SystemSQLCodec.TablePrefix(bootstrap.TestingUserDescID(0))
	args := adminSplitArgs(keyPrefix)
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), args)
	require.NoError(t, pErr.GoError())

	// Verify empty range has empty stats.
	repl := store.LookupReplica(roachpb.RKey(keyPrefix))
	assertRangeStats(t, "empty stats", store.TODOEngine(), repl.RangeID, enginepb.MVCCStats{})

	// Write random data.
	splitKey := kvserver.WriteRandomDataToRange(t, store, repl.RangeID, keyPrefix)

	start := s.Clock().Now()

	// Get the range stats now that we have data.
	// NB: need raft lock so that in-mem and disk stats can be expected to match.
	// See: https://github.com/cockroachdb/cockroach/issues/129601#issuecomment-2309865742
	repl.RaftLock()
	replMS := repl.GetMVCCStats()
	snap := store.TODOEngine().NewSnapshot()
	defer snap.Close()
	repl.RaftUnlock()

	ms, err := stateloader.Make(repl.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)
	assertRecomputedStats(t, "before split", snap, repl.Desc(), ms, start.WallTime)

	require.Equal(t, replMS, ms, "in-memory and on-disk stats diverge")

	// Split the range at approximate halfway point.
	// Call AdminSplit on the replica directly so that we can pass a
	// reason. Splits with reason "manual" bypass estimated stats.
	_, pErr = repl.AdminSplit(ctx, *adminSplitArgs(splitKey), "test")
	require.NoError(t, pErr.GoError())

	snap = store.TODOEngine().NewSnapshot()
	defer snap.Close()
	msLeft, err := stateloader.Make(repl.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)
	replRight := store.LookupReplica(splitKey)
	msRight, err := stateloader.Make(replRight.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)

	// Stats should both have the new timestamp.
	require.Less(t, start.WallTime, msLeft.LastUpdateNanos, "LHS stats have old timestamp")
	require.Less(t, start.WallTime, msRight.LastUpdateNanos, "RHS stats have old timestamp")

	// We don't care about system data.
	ms.SysBytes, ms.SysCount, ms.AbortSpanBytes = 0, 0, 0

	// The point key stats should be exactly equal when added.
	pointMS := ms
	pointMS.LastUpdateNanos = 0
	pointMS.RangeKeyCount, pointMS.RangeKeyBytes = 0, 0
	pointMS.RangeValCount, pointMS.RangeValBytes = 0, 0
	pointMS.GCBytesAge = 0
	expMS := enginepb.MVCCStats{
		LiveBytes:   msLeft.LiveBytes + msRight.LiveBytes,
		KeyBytes:    msLeft.KeyBytes + msRight.KeyBytes,
		ValBytes:    msLeft.ValBytes + msRight.ValBytes,
		IntentBytes: msLeft.IntentBytes + msRight.IntentBytes,
		LiveCount:   msLeft.LiveCount + msRight.LiveCount,
		KeyCount:    msLeft.KeyCount + msRight.KeyCount,
		ValCount:    msLeft.ValCount + msRight.ValCount,
		IntentCount: msLeft.IntentCount + msRight.IntentCount,
	}
	require.Equal(t, expMS, pointMS, "left plus right point key stats does not match original")

	// The range key stats should be equal or greater.
	require.GreaterOrEqual(t, msLeft.RangeKeyCount+msRight.RangeKeyCount, ms.RangeKeyCount)
	require.GreaterOrEqual(t, msLeft.RangeKeyBytes+msRight.RangeKeyBytes, ms.RangeKeyBytes)
	require.GreaterOrEqual(t, msLeft.RangeValCount+msRight.RangeValCount, ms.RangeValCount)
	require.GreaterOrEqual(t, msLeft.RangeValBytes+msRight.RangeValBytes, ms.RangeValBytes)
	require.GreaterOrEqual(t, msLeft.GCBytesAge+msRight.GCBytesAge, ms.GCBytesAge)

	// Stats should agree with re-computation.
	assertRecomputedStats(t, "LHS after split", snap, repl.Desc(), msLeft, s.Clock().PhysicalNow())
	assertRecomputedStats(t, "RHS after split", snap, replRight.Desc(), msRight, s.Clock().PhysicalNow())
	// Expect estimates if the cluster setting is enabled and neither side is empty.
	expectEstimates := kvserver.EnableEstimatedMVCCStatsInSplit.Get(&store.ClusterSettings().SV) &&
		msLeft.Total() > 0 && msRight.Total() > 0
	if expectEstimates {
		// The LHS gets ContainsEstimates = 1 from the split, which is then multiplied
		// by 2 in evaluateProposal for migration reasons.
		require.Equal(t, msLeft.ContainsEstimates, int64(2))
		// The RHS gets ContainsEstimates = 1 from the split.
		require.Equal(t, msRight.ContainsEstimates, int64(1))
	}

	// Merge the ranges back together, and assert that the merged stats
	// agree with the pre-split stats.
	mergeKey := repl.Desc().StartKey.AsRawKey()
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), adminMergeArgs(mergeKey))
	require.NoError(t, pErr.GoError())

	repl = store.LookupReplica(roachpb.RKey(keyPrefix))
	snap = store.TODOEngine().NewSnapshot()
	defer snap.Close()

	msMerged, err := stateloader.Make(repl.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)
	assertRecomputedStats(t, "in-mem after merge", snap, repl.Desc(), msMerged, s.Clock().PhysicalNow())

	msMerged.SysBytes, msMerged.SysCount, msMerged.AbortSpanBytes = 0, 0, 0
	ms.AgeTo(msMerged.LastUpdateNanos)
	if expectEstimates {
		// The stats delta accumulated by the merge has ContainsEstimates = 1 (from the
		// RHS stats). It gets multiplied by 2 in evaluateProposal for migration
		// reasons, and then added to the LHS stats, which has ContainsEstimates = 2
		// from above.
		ms.ContainsEstimates = 4
	}
	require.Equal(t, ms, msMerged, "post-merge stats differ from pre-split")
}

// TestStoreRangeSplitWithConcurrentWrites tests the behavior of splits with
// concurrent writes; in particular, it ensures that the MVCC stats for the two
// new ranges are correct.
//
// The test writes some data, then initiates a split and pauses it before the
// EndTxn request with the split trigger is evaluated. Then, it writes some more
// data, and unpauses the split. The MVCC stats for the two new ranges should
// agree with a re-computation. If the split computes accurate stats, the MVCC
// stats for the two new ranges should agree with a re-computation. Otherwise,
// we expect ContainsEstimates.
func TestStoreRangeSplitWithConcurrentWrites(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	splitKey := roachpb.Key("b")
	splitBlocked := make(chan struct{})
	filter := func(ctx context.Context, request *kvpb.BatchRequest) *kvpb.Error {
		if req, ok := request.GetArg(kvpb.EndTxn); ok {
			et := req.(*kvpb.EndTxnRequest)
			if tr := et.InternalCommitTrigger.GetSplitTrigger(); tr != nil {
				if tr.RightDesc.StartKey.Equal(splitKey) {
					// Signal that the split is blocked.
					splitBlocked <- struct{}{}
					// Wait for split to be unblocked.
					<-splitBlocked
				}
			}
		}
		return nil
	}

	testutils.RunTrueAndFalse(t, "estimates", func(t *testing.T, estimates bool) {
		testutils.RunTrueAndFalse(t, "recompute", func(t *testing.T, recompute bool) {
			testutils.RunTrueAndFalse(t, "maxCount", func(t *testing.T, maxCount bool) {
				testutils.RunTrueAndFalse(t, "maxBytes", func(t *testing.T, maxBytes bool) {
					ctx := context.Background()
					settings := cluster.MakeTestingClusterSettings()
					kvserver.EnableEstimatedMVCCStatsInSplit.Override(ctx, &settings.SV, estimates)
					kvserver.EnableMVCCStatsRecomputationInSplit.Override(ctx, &settings.SV, recompute)
					if maxCount {
						// If there is even a single write concurrent with the split, fall
						// back to accurate stats computation.
						batcheval.MaxMVCCStatCountDiff.Override(ctx, &settings.SV, 0)
					}
					if maxBytes {
						// If there are more than 10 bytes of writes concurrent with the split, fall
						// back to accurate stats computation.
						batcheval.MaxMVCCStatBytesDiff.Override(ctx, &settings.SV, 10)
					}

					s := serverutils.StartServerOnly(t, base.TestServerArgs{
						Knobs: base.TestingKnobs{
							Store: &kvserver.StoreTestingKnobs{
								DisableMergeQueue:       true,
								DisableSplitQueue:       true,
								DisableConsistencyQueue: true,
								TestingRequestFilter:    filter,
							},
						},
						Settings: settings,
					})

					defer s.Stopper().Stop(ctx)
					store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
					require.NoError(t, err)

					// Write some initial data to the future LHS.
					_, pErr := kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("a"), []byte("foo")))
					require.NoError(t, pErr.GoError())
					// Write some initial data to the future RHS.
					_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("c"), []byte("bar")))
					require.NoError(t, pErr.GoError())

					splitKeyAddr, err := keys.Addr(splitKey)
					require.NoError(t, err)
					lhsRepl := store.LookupReplica(splitKeyAddr)

					// Split the range.
					g := ctxgroup.WithContext(ctx)
					g.GoCtx(func(ctx context.Context) error {
						// Call AdminSplit on the replica directly so that we can pass a
						// reason. Splits with reason "manual" bypass estimated stats.
						_, pErr := lhsRepl.AdminSplit(ctx, *adminSplitArgs(splitKey), "test")
						return pErr.GoError()
					})

					// Wait until split is underway.
					<-splitBlocked

					// Write some more data to both sides.
					_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("aa"), []byte("foo")))
					require.NoError(t, pErr.GoError())
					_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("cc"), []byte("bar")))
					require.NoError(t, pErr.GoError())

					// Unblock the split.
					splitBlocked <- struct{}{}

					// Wait for the split to complete.
					require.Nil(t, g.Wait())

					snap := store.TODOEngine().NewSnapshot()
					defer snap.Close()
					lhsStats, err := stateloader.Make(lhsRepl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)
					rhsRepl := store.LookupReplica(splitKeyAddr)
					rhsStats, err := stateloader.Make(rhsRepl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)
					// If the split is producing estimates and neither of the tight count
					// and bytes thresholds is set, expect non-zero ContainsEstimates.
					expectContainsEstimates := estimates && !(maxCount || maxBytes)
					if expectContainsEstimates {
						require.Greater(t, lhsStats.ContainsEstimates, int64(0))
						require.Greater(t, rhsStats.ContainsEstimates, int64(0))
					} else {
						// Otherwise, the stats should agree with re-computation.
						assertRecomputedStats(t, "LHS after split", snap, lhsRepl.Desc(), lhsStats, s.Clock().PhysicalNow())
						assertRecomputedStats(t, "RHS after split", snap, rhsRepl.Desc(), rhsStats, s.Clock().PhysicalNow())
					}

					// If we used estimated stats while splitting the range, the stats on disk
					// will not match the stats recomputed from the range. We expect both of the
					// concurrent writes to be attributed to the RHS (instead of 1 to the LHS and
					// 1 th the RHS). But if we split these ranges one more time (no concurrent
					// writes this time), we expect the stats to be corrected and not
					// drift (when recompute = true).
					splitKeyLeft := roachpb.Key("aa")
					splitKeyLeftAddr, err := keys.Addr(splitKeyLeft)
					require.NoError(t, err)
					_, pErr = lhsRepl.AdminSplit(ctx, *adminSplitArgs(splitKeyLeft), "test")
					require.NoError(t, pErr.GoError())
					splitKeyRight := roachpb.Key("bb")
					splitKeyRightAddr, err := keys.Addr(splitKeyRight)
					require.NoError(t, err)
					_, pErr = rhsRepl.AdminSplit(ctx, *adminSplitArgs(splitKeyRight), "test")
					require.NoError(t, pErr.GoError())

					snap = store.TODOEngine().NewSnapshot()
					defer snap.Close()
					lhs1Stats, err := stateloader.Make(lhsRepl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)
					lhs2Repl := store.LookupReplica(splitKeyLeftAddr)
					lhs2Stats, err := stateloader.Make(lhs2Repl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)
					rhs1Stats, err := stateloader.Make(rhsRepl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)
					rhs2Repl := store.LookupReplica(splitKeyRightAddr)
					rhs2Stats, err := stateloader.Make(rhs2Repl.RangeID).LoadMVCCStats(ctx, snap)
					require.NoError(t, err)

					// Stats should agree with re-computation unless we're producing
					// estimates and not re-computing stats at the beginning of splits.
					expectIncorrectStats := expectContainsEstimates && !recompute
					if !expectIncorrectStats {
						assertRecomputedStats(t, "LHS1 after second split", snap, lhsRepl.Desc(), lhs1Stats, s.Clock().PhysicalNow())
						assertRecomputedStats(t, "LHS2 after second split", snap, lhs2Repl.Desc(), lhs2Stats, s.Clock().PhysicalNow())
						assertRecomputedStats(t, "RHS1 after second split", snap, rhsRepl.Desc(), rhs1Stats, s.Clock().PhysicalNow())
						assertRecomputedStats(t, "RHS2 after second split", snap, rhs2Repl.Desc(), rhs2Stats, s.Clock().PhysicalNow())
					} else {
						require.Greater(t, lhs1Stats.ContainsEstimates, int64(0))
						require.Greater(t, lhs2Stats.ContainsEstimates, int64(0))
						// The range corresponding to rhs1Stats is empty, so the split of the
						// original RHS fell back to accurate-stats computation.
						require.Equal(t, int64(0), rhs1Stats.ContainsEstimates)
						require.Equal(t, int64(0), rhs2Stats.ContainsEstimates)
					}
				})
			})
		})
	})
}

// TestStoreRangeSplitWithTracing tests that the split queue logs traces for
// slow splits.
func TestStoreRangeSplitWithTracing(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	l := log.ScopeWithoutShowLogs(t)
	_ = log.SetVModule("split_queue=1")
	defer l.Close(t)

	splitKey := roachpb.Key("b")
	var targetRange atomic.Int32
	manualClock := hlc.NewHybridManualClock()
	filter := func(ctx context.Context, request *kvpb.BatchRequest) *kvpb.Error {
		if req, ok := request.GetArg(kvpb.EndTxn); ok {
			et := req.(*kvpb.EndTxnRequest)
			if tr := et.InternalCommitTrigger.GetSplitTrigger(); tr != nil {
				if tr.RightDesc.StartKey.Equal(splitKey) {
					// Manually increment the replica's clock to simulate a slow split.
					manualClock.Increment(kvserver.SlowSplitTracingThreshold.Default().Nanoseconds())
				}
			}
		}
		return nil
	}

	// Override the load-based split key funciton to force the range to be
	// processed by the split queue.
	overrideLBSplitFn := func(rangeID roachpb.RangeID) (roachpb.Key, bool) {
		if rangeID == roachpb.RangeID(targetRange.Load()) {
			return splitKey, true
		}
		return nil, false
	}

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Server: &server.TestingKnobs{
				WallClock: manualClock,
			},
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:             true,
				DisableSplitQueue:             true,
				TestingRequestFilter:          filter,
				LoadBasedSplittingOverrideKey: overrideLBSplitFn,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Write some data on both sides of the split key.
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("a"), []byte("foo")))
	require.NoError(t, pErr.GoError())
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs(splitKey, []byte("bar")))
	require.NoError(t, pErr.GoError())
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("c"), []byte("foo")))
	require.NoError(t, pErr.GoError())

	splitKeyAddr, err := keys.Addr(splitKey)
	require.NoError(t, err)
	repl := store.LookupReplica(splitKeyAddr)
	targetRange.Store(int32(repl.RangeID))

	traceCtx, rec := tracing.ContextWithRecordingSpan(ctx, store.GetStoreConfig().Tracer(), "trace-enqueue")
	processErr, enqueueErr := store.Enqueue(
		traceCtx, "split", repl, true /* skipShouldQueue */, false, /* async */
	)
	recording := rec()
	require.NoError(t, enqueueErr)
	require.NoError(t, processErr)

	// Flush logs and get log messages from split_queue.go
	log.FlushFiles()
	entries, err := log.FetchEntriesFromFiles(math.MinInt64, math.MaxInt64, 100,
		regexp.MustCompile(`split_queue\.go`), log.WithMarkedSensitiveData)
	require.NoError(t, err)

	opName := "split"
	traceRegexp, err := regexp.Compile(`trace:.*`)
	require.NoError(t, err)
	opRegexp, err := regexp.Compile(fmt.Sprintf(`operation:%s`, opName))
	require.NoError(t, err)

	// Find the log entry to validate the trace output.
	foundEntry := false
	var entry logpb.Entry
	for _, entry = range entries {
		if opRegexp.MatchString(entry.Message) {
			foundEntry = true
			break
		}
	}
	require.True(t, foundEntry)

	// Validate that the trace is included in the log message.
	require.Regexp(t, traceRegexp, entry.Message)

	// Validate that the returned tracing span includes the operation, but also
	// that the stringified trace was not logged to the span or its parent.
	processRecSpan, foundSpan := recording.FindSpan(opName)
	require.True(t, foundSpan)

	foundParent := false
	var parentRecSpan tracingpb.RecordedSpan
	for _, parentRecSpan = range recording {
		if parentRecSpan.SpanID == processRecSpan.ParentSpanID {
			foundParent = true
			break
		}
	}
	require.True(t, foundParent)
	spans := tracingpb.Recording{parentRecSpan, processRecSpan}
	stringifiedSpans := spans.String()
	require.NotRegexp(t, traceRegexp, stringifiedSpans)
}

// TestStoreRangeSplitWithMismatchedDesc ensures that if a RecomputeRequest
// during a split fails due to a range descriptor mismatch, the split doesn't
// return a non-retryable error, which can be surfaced to the client.
// The test works by splitting a range and concurrently subsuming that range via
// a range merge. The split returns a RangeNotFoundError, which will be retried
// by the DistSender.
func TestStoreRangeSplitWithMismatchedDesc(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	splitBlocked := make(chan struct{})
	doneBlocking := atomicValue[bool]{}
	doneBlocking.set(false)
	filter := func(ctx context.Context, request *kvpb.BatchRequest) *kvpb.Error {
		if req, ok := request.GetArg(kvpb.RecomputeStats); ok {
			rs := req.(*kvpb.RecomputeStatsRequest)
			// Block the split right before evaluating the RecomputeStats request.
			// Only do so the first time; the split may get retried or there could be
			// other RecomputeStatsRequests, and we don't want to block them.
			if rs.Key.Equal(roachpb.Key("b")) && !doneBlocking.get() {
				// Signal that the split is blocked.
				splitBlocked <- struct{}{}
				// Wait for split to be unblocked.
				<-splitBlocked
				doneBlocking.set(true)
			}
		}
		return nil
	}

	ctx := context.Background()
	settings := cluster.MakeClusterSettings()
	kvserver.EnableEstimatedMVCCStatsInSplit.Override(ctx, &settings.SV, true)
	kvserver.EnableMVCCStatsRecomputationInSplit.Override(ctx, &settings.SV, true)
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:    true,
				DisableSplitQueue:    true,
				TestingRequestFilter: filter,
			},
		},
		Settings: settings,
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Write some initial data.
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("a"), []byte("foo")))
	require.NoError(t, pErr.GoError())
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("b"), []byte("bar")))
	require.NoError(t, pErr.GoError())
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("c"), []byte("foo")))
	require.NoError(t, pErr.GoError())
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), putArgs([]byte("d"), []byte("bar")))
	require.NoError(t, pErr.GoError())

	lhsRepl := store.LookupReplica(roachpb.RKey("a"))
	// Split the range at "b".
	_, pErr = lhsRepl.AdminSplit(ctx, *adminSplitArgs(roachpb.Key("b")), "test")
	require.NoError(t, pErr.GoError())

	rhsRepl := store.LookupReplica(roachpb.RKey("b"))
	// Split the new RHS at "c". This split will be blocked by the above filter
	// because the range we're splitting starts at key "b".
	g := ctxgroup.WithContext(ctx)
	g.GoCtx(func(ctx context.Context) error {
		// Use AdminSplit with a non-test sender here to reproduce exactly what will
		// be returned to the client.
		_, pErr = rhsRepl.AdminSplit(ctx, *adminSplitArgs(roachpb.Key("c")), "test")
		return pErr.GoError()
	})

	// Wait until split is underway.
	<-splitBlocked

	// Merge the LHS and RHS back into one range ("a" - "d").
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), adminMergeArgs(roachpb.Key("a")))
	require.NoError(t, pErr.GoError())

	// Unblock the split.
	splitBlocked <- struct{}{}

	// Wait for the split to complete and ensure a RangeNotFoundError is returned.
	// The split will hit a benign error because the range descriptor changed.
	// The DistSender will retry the RangeNotFoundError. The second time around,
	// the split is a no-op because the range "b" - "d" was subsumed.
	require.IsType(t, &kvpb.RangeNotFoundError{}, g.Wait())
}

// RaftMessageHandlerInterceptor wraps a storage.IncomingRaftMessageHandler. It
// delegates all methods to the underlying storage.IncomingRaftMessageHandler,
// except that HandleSnapshot calls receiveSnapshotFilter with the snapshot
// request header before delegating to the underlying HandleSnapshot method.
type RaftMessageHandlerInterceptor struct {
	kvserver.IncomingRaftMessageHandler
	handleSnapshotFilter func(header *kvserverpb.SnapshotRequest_Header)
}

func (mh RaftMessageHandlerInterceptor) HandleSnapshot(
	ctx context.Context,
	header *kvserverpb.SnapshotRequest_Header,
	respStream kvserver.SnapshotResponseStream,
) error {
	mh.handleSnapshotFilter(header)
	return mh.IncomingRaftMessageHandler.HandleSnapshot(ctx, header, respStream)
}

// TestStoreEmptyRangeSnapshotSize tests that the snapshot request header for a
// range that contains no user data (an "empty" range) has RangeSize == 0. This
// is arguably a bug, because system data like the range descriptor and raft log
// should also count towards the size of the snapshot. Currently, though, this
// property conveniently allows us to optimize the rebalancing of empty ranges
// by throttling snapshots of empty ranges separately from non-empty snapshots.
//
// If you change the accounting of RangeSize such that this test breaks, please
// preserve the optimization by introducing an alternative means of identifying
// snapshot requests for empty or near-empty ranges, and then adjust this test
// accordingly.
func TestStoreEmptyRangeSnapshotSize(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 2,
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
		})
	defer tc.Stopper().Stop(ctx)

	// Split the range after the last table data key to get a range that contains
	// no user data.
	splitKey := keys.SystemSQLCodec.TablePrefix(bootstrap.TestingUserDescID(0))
	splitArgs := adminSplitArgs(splitKey)
	if _, err := kv.SendWrapped(ctx, tc.Servers[0].DistSenderI().(kv.Sender), splitArgs); err != nil {
		t.Fatal(err)
	}

	// Wrap store 1's message handler to intercept and record all incoming
	// snapshot request headers.
	messageRecorder := struct {
		syncutil.Mutex
		headers []*kvserverpb.SnapshotRequest_Header
	}{}
	messageHandler := RaftMessageHandlerInterceptor{
		IncomingRaftMessageHandler: tc.GetFirstStoreFromServer(t, 1),
		handleSnapshotFilter: func(header *kvserverpb.SnapshotRequest_Header) {
			// Each snapshot request is handled in a new goroutine, so we need
			// synchronization.
			messageRecorder.Lock()
			defer messageRecorder.Unlock()
			messageRecorder.headers = append(messageRecorder.headers, header)
		},
	}
	tc.Servers[1].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(tc.GetFirstStoreFromServer(t, 1).StoreID(), messageHandler)

	// Replicate the newly-split range to trigger a snapshot request from store 0
	// to store 1.
	desc := tc.AddVotersOrFatal(t, splitKey, tc.Target(1))

	// Verify that we saw at least one snapshot request,
	messageRecorder.Lock()
	defer messageRecorder.Unlock()
	if a := len(messageRecorder.headers); a < 1 {
		t.Fatalf("expected at least one snapshot header, but got %d", a)
	}
	for i, header := range messageRecorder.headers {
		if e, a := header.State.Desc.RangeID, desc.RangeID; e != a {
			t.Errorf("%d: expected RangeID to be %d, but got %d", i, e, a)
		}
		if header.RangeSize != 0 {
			t.Errorf("%d: expected RangeSize to be 0, but got %d", i, header.RangeSize)
		}
	}
}

// TestStoreRangeSplitStatsWithMerges starts by splitting the system keys from
// user-space keys and verifying that the user space side of the split (which is empty),
// has all zeros for stats. It then issues a number of Merge requests to the user
// space side, simulating TimeSeries data. Finally, the test splits the user space
// side halfway and verifies the stats on either side of the split are equal to a
// recomputation.
//
// Note that unlike TestStoreRangeSplitStats, we do not check if the two halves of the
// split's stats are equal to the pre-split stats when added, because this will not be
// true of ranges populated with Merge requests. The reason for this is that Merge
// requests' impact on MVCCStats are only estimated. See updateStatsOnMerge.
func TestStoreRangeSplitStatsWithMerges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableSplitQueue: true,
				DisableMergeQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	start := s.Clock().Now()

	// Split the range after the last table data key.
	keyPrefix := keys.SystemSQLCodec.TablePrefix(bootstrap.TestingUserDescID(0))
	args := adminSplitArgs(keyPrefix)
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), args)
	require.NoError(t, pErr.GoError())

	// Verify empty range has empty stats.
	repl := store.LookupReplica(roachpb.RKey(keyPrefix))
	// NOTE that this value is expected to change over time, depending on what
	// we store in the sys-local keyspace. Update it accordingly for this test.
	empty := enginepb.MVCCStats{LastUpdateNanos: start.WallTime}
	assertRangeStats(t, "empty stats", store.TODOEngine(), repl.RangeID, empty)

	// Write random TimeSeries data.
	midKey := writeRandomTimeSeriesDataToRange(t, store, repl.RangeID, keyPrefix)

	// Split the range at approximate halfway point.
	// Call AdminSplit on the replica directly so that we can pass a
	// reason. Splits with reason "manual" bypass estimated stats.
	_, pErr = repl.AdminSplit(ctx, *adminSplitArgs(midKey), "test")
	require.NoError(t, pErr.GoError())

	snap := store.TODOEngine().NewSnapshot()
	defer snap.Close()
	msLeft, err := stateloader.Make(repl.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)
	replRight := store.LookupReplica(midKey)
	msRight, err := stateloader.Make(replRight.RangeID).LoadMVCCStats(ctx, snap)
	require.NoError(t, err)

	// Stats should both have the new timestamp.
	require.Less(t, start.WallTime, msLeft.LastUpdateNanos, "LHS stats have old timestamp")
	require.Less(t, start.WallTime, msRight.LastUpdateNanos, "RHS stats have old timestamp")

	// Stats should agree with re-computation because generally the split will
	// correct the estimated time-series stats. This is not true if the split is
	// estimating stats and not re-computing them in AdminSplit.
	estimates := kvserver.EnableEstimatedMVCCStatsInSplit.Get(&store.ClusterSettings().SV)
	recompute := kvserver.EnableMVCCStatsRecomputationInSplit.Get(&store.ClusterSettings().SV)
	if !estimates || (estimates && recompute) {
		assertRecomputedStats(t, "LHS after split", snap, repl.Desc(), msLeft, s.Clock().PhysicalNow())
		assertRecomputedStats(t, "RHS after split", snap, replRight.Desc(), msRight, s.Clock().PhysicalNow())
	}
}

// fillRange writes keys with the given prefix and associated values
// until bytes bytes have been written or the given range has split.
func fillRange(
	t *testing.T,
	store *kvserver.Store,
	rangeID roachpb.RangeID,
	prefix roachpb.Key,
	bytes int64,
	singleKey bool,
) {
	src := rand.New(rand.NewSource(0))
	var key []byte
	for {
		ms, err := stateloader.Make(rangeID).LoadMVCCStats(context.Background(), store.TODOEngine())
		if err != nil {
			t.Fatal(err)
		}
		keyBytes, valBytes := ms.KeyBytes, ms.ValBytes
		if keyBytes+valBytes >= bytes {
			return
		}
		if key == nil || !singleKey {
			key = append(append([]byte(nil), prefix...), randutil.RandBytes(src, 1000)...)
			key = keys.MakeFamilyKey(key, src.Uint32())
		}
		val := randutil.RandBytes(src, 200000)
		pArgs := putArgs(key, val)
		_, pErr := kv.SendWrappedWith(context.Background(), store, kvpb.Header{
			RangeID: rangeID,
		}, pArgs)
		// When the split occurs in the background, our writes may start failing.
		// We know we can stop writing when this happens.
		if _, ok := pErr.GetDetail().(*kvpb.RangeKeyMismatchError); ok {
			return
		} else if pErr != nil {
			t.Fatal(pErr)
		}
	}
}

// TestStoreZoneUpdateAndRangeSplit verifies that modifying the zone
// configuration changes range max bytes and Range.maybeSplit() takes
// max bytes into account when deciding whether to enqueue a range for
// splitting. It further verifies that the range is in fact split on
// exceeding zone's RangeMaxBytes.
func TestStoreZoneUpdateAndRangeSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	tdb := sqlutils.MakeSQLRunner(sqlDB)
	tdb.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '20ms'`)
	tdb.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '20ms'`)
	tdb.Exec(t, `SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '20ms'`)
	tdb.Exec(t, `SET CLUSTER SETTING kv.range_split.by_load_enabled = false`)
	tdb.Exec(t, "CREATE TABLE t ()")
	var descID uint32
	tdb.QueryRow(t, "SELECT 't'::regclass::int").Scan(&descID)
	const maxBytes, minBytes = 100 << 20, 1 << 14
	tdb.Exec(t, "ALTER TABLE t CONFIGURE ZONE USING range_max_bytes = $1, range_min_bytes = $2",
		maxBytes, minBytes)

	tableBoundary := keys.SystemSQLCodec.TablePrefix(descID)
	{
		var repl *kvserver.Replica

		// Wait for the range to be split along table boundaries.
		expectedRSpan := roachpb.RSpan{Key: roachpb.RKey(tableBoundary), EndKey: roachpb.RKeyMax}
		testutils.SucceedsSoon(t, func() error {
			repl = store.LookupReplica(roachpb.RKey(tableBoundary))
			if actualRSpan := repl.Desc().RSpan(); !actualRSpan.Equal(expectedRSpan) {
				return errors.Errorf("expected range %s to span %s", repl, expectedRSpan)
			}
			// Check range's max bytes settings.
			if actualMaxBytes := repl.GetMaxBytes(ctx); actualMaxBytes != maxBytes {
				return errors.Errorf("range %s max bytes mismatch, got: %d, expected: %d", repl, actualMaxBytes, maxBytes)
			}
			return nil
		})

		// Look in the range after prefix we're writing to.
		fillRange(t, store, repl.RangeID, tableBoundary, maxBytes, false /* singleKey */)
	}

	// Verify that the range is in fact split.
	testutils.SucceedsSoon(t, func() error {
		repl := store.LookupReplica(roachpb.RKey(keys.SystemSQLCodec.TablePrefix(descID + 1)))
		rngDesc := repl.Desc()
		rngStart, rngEnd := rngDesc.StartKey, rngDesc.EndKey
		if rngStart.Equal(tableBoundary) || !rngEnd.Equal(roachpb.RKeyMax) {
			return errors.Errorf("range %s has not yet split", repl)
		}
		return nil
	})
}

// TestStoreRangeSplitWithMaxBytesUpdate tests a scenario where a new
// zone config that updates the max bytes is set and triggers a range
// split.
func TestStoreRangeSplitWithMaxBytesUpdate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Find the last range.
	var max roachpb.RKey
	var origRng *roachpb.RangeDescriptor
	store.VisitReplicas(func(replica *kvserver.Replica) (wantMore bool) {
		if rd := replica.Desc(); max == nil || max.Less(rd.StartKey) {
			origRng = rd
			max = rd.StartKey
		}
		return true
	})

	// Create a new table and configure its size.
	tdb := sqlutils.MakeSQLRunner(sqlDB)
	tdb.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.target_duration = '20ms'`)
	tdb.Exec(t, `SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '20ms'`)
	tdb.Exec(t, `SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '20ms'`)
	tdb.Exec(t, "CREATE TABLE t ()")
	var descID uint32
	tdb.QueryRow(t, "SELECT 't'::regclass::int").Scan(&descID)
	const maxBytes, minBytes = 100 << 20, 1 << 14
	tdb.Exec(t, "ALTER TABLE t CONFIGURE ZONE USING range_max_bytes = $1, range_min_bytes = $2",
		maxBytes, minBytes)

	// Verify that the range is split and the new range has the correct max bytes.
	testutils.SucceedsSoon(t, func() error {
		newRng := store.LookupReplica(roachpb.RKey(keys.SystemSQLCodec.TablePrefix(descID)))
		if newRng == nil {
			return errors.Errorf("expected new range created by split")
		}
		if newRng.RangeID == origRng.RangeID {
			return errors.Errorf("expected new range created by split")
		}
		if newRng.GetMaxBytes(ctx) != maxBytes {
			return errors.Errorf("expected %d max bytes for the new range, but got %d",
				maxBytes, newRng.GetMaxBytes(ctx))
		}
		return nil
	})
}

// TestStoreRangeSplitBackpressureWrites tests that ranges that grow too large
// begin enforcing backpressure on writes until the range is able to split. In
// the test, a range is filled past the point where it will begin applying
// backpressure. Splits are then blocked in-flight and we test that any future
// writes wait until the split succeeds and reduces the range size beneath the
// backpressure threshold.
func TestStoreRangeSplitBackpressureWrites(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Backpressured writes react differently depending on whether there is an
	// ongoing split or not. If there is an ongoing split then the writes wait
	// on the split are only allowed to proceed if the split succeeds. If there
	// is not an ongoing split or if the range is unsplittable and in the split
	// queue's purgatory, the write is rejected immediately.
	testCases := []struct {
		splitOngoing    bool
		splitErr        bool
		splitImpossible bool
		expErr          string
	}{
		{splitOngoing: true, splitErr: false, expErr: ""},
		{splitOngoing: true, splitErr: true, expErr: "split failed while applying backpressure.* boom"},
		{splitOngoing: false, expErr: ""},
		{splitImpossible: true, expErr: "split failed while applying backpressure.* could not find valid split key"},
	}
	for _, tc := range testCases {
		var name string
		if tc.splitImpossible {
			name = fmt.Sprintf("splitImpossible=%t", tc.splitImpossible)
		} else {
			name = fmt.Sprintf("splitOngoing=%t,splitErr=%t", tc.splitOngoing, tc.splitErr)
		}
		t.Run(name, func(t *testing.T) {
			var activateSplitFilter int32
			splitKey := roachpb.RKey(bootstrap.TestingUserTableDataMin(keys.SystemSQLCodec))
			splitPending, blockSplits := make(chan struct{}), make(chan struct{})

			// Set maxBytes to something small so we can exceed the maximum split
			// size without adding 2x64MB of data.
			defer zonepb.TestingSetMinRangeMaxBytes(1 << 16)()
			const minBytes = 1 << 12
			const maxBytes = 1 << 18
			zoneConfig := zonepb.DefaultZoneConfig()
			zoneConfig.RangeMinBytes = proto.Int64(minBytes)
			zoneConfig.RangeMaxBytes = proto.Int64(maxBytes)
			zoneConfig.NumReplicas = proto.Int32(1)

			testingRequestFilter :=
				func(_ context.Context, ba *kvpb.BatchRequest) *kvpb.Error {
					for _, req := range ba.Requests {
						if cPut, ok := req.GetInner().(*kvpb.ConditionalPutRequest); ok {
							if cPut.Key.Equal(keys.RangeDescriptorKey(splitKey)) {
								if atomic.CompareAndSwapInt32(&activateSplitFilter, 1, 0) {
									splitPending <- struct{}{}
									<-blockSplits
									if tc.splitErr {
										return kvpb.NewErrorf("boom")
									}
								}
							}
						}
					}
					return nil
				}

			ctx := context.Background()
			s := serverutils.StartServerOnly(t, base.TestServerArgs{
				Knobs: base.TestingKnobs{
					Server: &server.TestingKnobs{
						DefaultZoneConfigOverride: &zoneConfig,
					},
					Store: &kvserver.StoreTestingKnobs{
						DisableCanAckBeforeApplication: true,
						DisableGCQueue:                 true,
						DisableMergeQueue:              true,
						DisableSplitQueue:              true,
						TestingRequestFilter:           testingRequestFilter,
					},
				},
			})

			defer s.Stopper().Stop(ctx)
			store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
			require.NoError(t, err)

			// Split at the split key.
			sArgs := adminSplitArgs(splitKey.AsRawKey())
			repl := store.LookupReplica(splitKey)
			if _, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{
				RangeID: repl.RangeID,
			}, sArgs); pErr != nil {
				t.Fatal(pErr)
			}

			// Fill the new range past the point where writes should backpressure.
			repl = store.LookupReplica(splitKey)
			singleKey := tc.splitImpossible
			fillRange(t, store, repl.RangeID, splitKey.AsRawKey(), 2*maxBytes+1, singleKey)

			if !repl.ShouldBackpressureWrites(ctx) {
				t.Fatal("expected ShouldBackpressureWrites=true, found false")
			}

			// If necessary, allow the range to begin splitting and wait until
			// it gets blocked in the response filter.
			if tc.splitOngoing {
				atomic.StoreInt32(&activateSplitFilter, 1)
				if err := s.Stopper().RunAsyncTask(ctx, "force split", func(_ context.Context) {
					store.TestingSetSplitQueueActive(true)
					if err := store.ForceSplitScanAndProcess(); err != nil {
						log.Fatalf(ctx, "%v", err)
					}
				}); err != nil {
					t.Fatal(err)
				}
				<-splitPending
			} else if tc.splitImpossible {
				store.TestingSetSplitQueueActive(true)
				if err := store.ForceSplitScanAndProcess(); err != nil {
					t.Fatal(err)
				}
				if l := store.SplitQueuePurgatoryLength(); l != 1 {
					t.Fatalf("expected split queue purgatory to contain 1 replica, found %d", l)
				}
			}

			// Send a Put request. This should be backpressured on the split, so it should
			// not be able to succeed until we allow the split to continue.
			putRes := make(chan error)
			go func() {
				// Write to the first key of the range to make sure that
				// we don't end up on the wrong side of the split.
				putRes <- store.DB().Put(ctx, splitKey, "test")
			}()

			// Send a Delete request in a transaction. Should also be backpressured on the split,
			// so it should not be able to succeed until we allow the split to continue.
			delRes := make(chan error)
			go func() {
				// Write to the first key of the range to make sure that
				// we don't end up on the wrong side of the split.
				delRes <- store.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
					b := txn.NewBatch()
					b.Del(splitKey)
					return txn.CommitInBatch(ctx, b)
				})
			}()

			// Make sure the write doesn't return while a split is ongoing. If no
			// split is ongoing, the write will return an error immediately.
			if tc.splitOngoing {
				select {
				case err := <-putRes:
					close(blockSplits)
					t.Fatalf("put was not blocked on split, returned err %v", err)
				case err := <-delRes:
					close(blockSplits)
					t.Fatalf("delete was not blocked on split, returned err %v", err)
				case <-time.After(100 * time.Millisecond):
				}

				// Let split through. Write should follow.
				close(blockSplits)
			}

			for op, resCh := range map[string]chan error{
				"put":    putRes,
				"delete": delRes,
			} {
				if err := <-resCh; tc.expErr == "" {
					if err != nil {
						t.Fatalf("%s returned err %v, expected success", op, err)
					}
				} else {
					if !testutils.IsError(err, tc.expErr) {
						t.Fatalf("%s returned err %s, expected pattern %q", op, err, tc.expErr)
					}
				}
			}

		})
	}
}

// runSetupSplitSnapshotRace engineers a situation in which a range has
// been split but node 3 hasn't processed it yet. There is a race
// depending on whether node 3 learns of the split from its left or
// right side. When this function returns most of the nodes will be
// stopped, and depending on the order in which they are restarted, we
// can arrange for both possible outcomes of the race.
//
// Range 1 is the system keyspace, located on node 0.
//
// The range containing leftKey is the left side of the split, located
// on nodes 1, 2, and 3.
//
// The range containing rightKey is the right side of the split,
// located on nodes 3, 4, and 5.
//
// Nodes 1-5 are stopped; only node 0 is running.
//
// See https://github.com/cockroachdb/cockroach/issues/1644.
func runSetupSplitSnapshotRace(
	t *testing.T,
	stickyVFSRegistry fs.StickyRegistry,
	testFn func(*testcluster.TestCluster, roachpb.Key, roachpb.Key),
) {
	const numServers int = 6
	stickyServerArgs := make(map[int]base.TestServerArgs)
	for i := 0; i < numServers; i++ {
		stickyServerArgs[i] = base.TestServerArgs{
			StoreSpecs: []base.StoreSpec{
				{
					InMemory:    true,
					StickyVFSID: strconv.FormatInt(int64(i), 10),
				},
			},
			Knobs: base.TestingKnobs{
				Server: &server.TestingKnobs{
					StickyVFSRegistry: stickyVFSRegistry,
				},
				Store: &kvserver.StoreTestingKnobs{
					DisableGCQueue: true,
					// Async intent resolution can sometimes lead to hangs when we stop
					// most of the stores at the end of this function.
					IntentResolverKnobs: kvserverbase.IntentResolverTestingKnobs{
						DisableAsyncIntentResolution: true,
					},
				},
			},
			RaftConfig: base.RaftConfig{
				// Disable the split delay mechanism, or it'll spend 10s going in circles.
				// (We can't set it to zero as otherwise the default overrides us).
				RaftDelaySplitToSuppressSnapshot: -1,
			},
		}
	}
	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, numServers,
		base.TestClusterArgs{
			ReplicationMode:   base.ReplicationManual,
			ServerArgsPerNode: stickyServerArgs,
		})
	defer tc.Stopper().Stop(ctx)
	store := tc.GetFirstStoreFromServer(t, 0)

	leftKey := roachpb.Key("a")
	rightKey := roachpb.Key("z")

	// First, do a couple of writes; we'll use these to determine when
	// the dust has settled.
	incArgs := incrementArgs(leftKey, 1)
	if _, pErr := kv.SendWrapped(context.Background(), store.TestSender(), incArgs); pErr != nil {
		t.Fatal(pErr)
	}
	incArgs = incrementArgs(rightKey, 2)
	if _, pErr := kv.SendWrapped(context.Background(), store.TestSender(), incArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Split the system range from the rest of the keyspace.
	splitArgs := adminSplitArgs(keys.SystemMax)
	if _, pErr := kv.SendWrapped(context.Background(), store.TestSender(), splitArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Get the left range replica.
	lhsRepl := store.LookupReplica(roachpb.RKey("a"))

	// Replicate the left range onto nodes 1-3 and remove it from node 0. We have
	// to transfer the lease before unreplicating from store 0 because it isn't
	// safe (or allowed) for a leaseholder to remove itself from a cluster
	// without first giving up its lease.
	desc := tc.AddVotersOrFatal(t, lhsRepl.Desc().StartKey.AsRawKey(), tc.Targets(1, 2, 3)...)
	tc.TransferRangeLeaseOrFatal(t, desc, tc.Target(1))
	tc.RemoveVotersOrFatal(t, lhsRepl.Desc().StartKey.AsRawKey(), tc.Target(0))

	tc.WaitForValues(t, leftKey, []int64{0, 1, 1, 1, 0, 0})
	tc.WaitForValues(t, rightKey, []int64{0, 2, 2, 2, 0, 0})

	// Stop node 3 so it doesn't hear about the split.
	tc.StopServer(3)

	// Split the data range.
	splitArgs = adminSplitArgs(roachpb.Key("m"))
	if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), splitArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Get the right range's ID. Since the split was performed on node
	// 1, it is currently 11 and not 3 as might be expected.
	var rhsRepl *kvserver.Replica
	testutils.SucceedsSoon(t, func() error {
		rhsRepl = tc.GetFirstStoreFromServer(t, 1).LookupReplica(roachpb.RKey("z"))
		if rhsRepl.RangeID == lhsRepl.RangeID {
			return errors.Errorf("store 1 hasn't processed split yet")
		}
		return nil
	})

	// Relocate the right range onto nodes 3-5.
	tc.AddVotersOrFatal(t, rhsRepl.Desc().StartKey.AsRawKey(), tc.Targets(4, 5)...)
	tc.RemoveVotersOrFatal(t, rhsRepl.Desc().StartKey.AsRawKey(), tc.Target(2))
	tc.TransferRangeLeaseOrFatal(t, *rhsRepl.Desc(), tc.Target(4))
	tc.RemoveVotersOrFatal(t, rhsRepl.Desc().StartKey.AsRawKey(), tc.Target(1))

	// Perform another increment after all the replication changes. This
	// lets us ensure that all the replication changes have been
	// processed and applied on all replicas. This is necessary because
	// the range is in an unstable state at the time of the last
	// unreplicateRange call above. It has four members which means it
	// can only tolerate one failure without losing quorum. That failure
	// is store 3 which we stopped earlier. Stopping store 1 too soon
	// (before it has committed the final config change *and* propagated
	// that commit to the followers 4 and 5) would constitute a second
	// failure and render the range unable to achieve quorum after
	// restart (in the SnapshotWins branch).
	incArgs = incrementArgs(rightKey, 3)
	if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Store 3 still has the old value (but it's offline), and 4 and 5
	// are up to date.
	tc.WaitForValues(t, rightKey, []int64{0, 0, 0, 0 /* stopped */, 5, 5})

	// Scan the meta ranges to resolve all intents
	if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender),
		&kvpb.ScanRequest{
			RequestHeader: kvpb.RequestHeader{
				Key:    keys.MetaMin,
				EndKey: keys.MetaMax,
			},
		}); pErr != nil {
		t.Fatal(pErr)
	}

	// Stop the remaining data stores.
	tc.StopServer(1)
	tc.StopServer(2)
	// 3 is already stopped.
	tc.StopServer(4)
	tc.StopServer(5)

	testFn(tc, leftKey, rightKey)
}

// TestSplitSnapshotRace_SplitWins exercises one outcome of the
// split/snapshot race: The left side of the split propagates first,
// so the split completes before it sees a competing snapshot. This is
// the more common outcome in practice.
func TestSplitSnapshotRace_SplitWins(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runSetupSplitSnapshotRace(t, fs.NewStickyRegistry(), func(tc *testcluster.TestCluster, leftKey, rightKey roachpb.Key) {
		// Bring the left range up first so that the split happens before it sees a snapshot.
		for i := 1; i <= 3; i++ {
			require.NoError(t, tc.RestartServer(i))
		}

		// Perform a write on the left range and wait for it to propagate.
		incArgs := incrementArgs(leftKey, 10)
		if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
			t.Fatal(pErr)
		}
		tc.WaitForValues(t, leftKey, []int64{0, 11, 11, 11, 0, 0})

		// Now wake the other stores up.
		require.NoError(t, tc.RestartServer(4))
		require.NoError(t, tc.RestartServer(5))

		// Write to the right range.
		incArgs = incrementArgs(rightKey, 20)
		if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
			t.Fatal(pErr)
		}
		tc.WaitForValues(t, rightKey, []int64{0, 0, 0, 25, 25, 25})
	})
}

// TestSplitSnapshotRace_SnapshotWins exercises one outcome of the
// split/snapshot race: The right side of the split replicates first,
// so the target node sees a raft snapshot before it has processed the
// split, so it still has a conflicting range.
func TestSplitSnapshotRace_SnapshotWins(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	runSetupSplitSnapshotRace(t, fs.NewStickyRegistry(), func(tc *testcluster.TestCluster, leftKey, rightKey roachpb.Key) {
		// Bring the right range up first.
		for i := 3; i <= 5; i++ {
			require.NoError(t, tc.RestartServer(i))
		}

		// Perform a write on the right range.
		incArgs := incrementArgs(rightKey, 20)
		if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
			t.Fatal(pErr)
		}

		// It immediately propagates between nodes 4 and 5, but node 3
		// remains at its old value. It can't accept the right-hand range
		// because it conflicts with its not-yet-split copy of the left-hand
		// range. This test is not completely deterministic: we want to make
		// sure that node 3 doesn't panic when it receives the snapshot, but
		// since it silently drops the message there is nothing we can wait
		// for. There is a high probability that the message will have been
		// received by the time that nodes 4 and 5 have processed their
		// update.
		tc.WaitForValues(t, rightKey, []int64{0, 0, 0, 2, 25, 25})

		// Wake up the left-hand range. This will allow the left-hand
		// range's split to complete and unblock the right-hand range.
		require.NoError(t, tc.RestartServer(1))
		require.NoError(t, tc.RestartServer(2))

		// Perform writes on both sides. This is not strictly necessary but
		// it helps wake up dormant ranges that would otherwise have to wait
		// for retry timeouts.
		incArgs = incrementArgs(leftKey, 10)
		if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
			t.Fatal(pErr)
		}
		tc.WaitForValues(t, leftKey, []int64{0, 11, 11, 11, 0, 0})

		incArgs = incrementArgs(rightKey, 200)
		if _, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), incArgs); pErr != nil {
			t.Fatal(pErr)
		}
		tc.WaitForValues(t, rightKey, []int64{0, 0, 0, 225, 225, 225})
	})
}

// TestStoreSplitTimestampCacheDifferentLeaseHolder prevents regression of
// #7899. When the first lease holder of the right-hand side of a Split was
// not equal to the left-hand side lease holder (at the time of the split),
// its timestamp cache would not be properly initialized, which would allow
// for writes which invalidated reads previously served by the pre-split lease.
func TestStoreSplitTimestampCacheDifferentLeaseHolder(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	leftKey := roachpb.Key("a")
	splitKey := roachpb.Key("b")
	rightKey := roachpb.Key("c")

	// This filter is better understood when reading the meat of the test
	// below first.
	var noLeaseForDesc atomic.Value
	filter := func(args kvserverbase.FilterArgs) *kvpb.Error {
		leaseReq, argOK := args.Req.(*kvpb.RequestLeaseRequest)
		forbiddenDesc, descOK := noLeaseForDesc.Load().(*roachpb.ReplicaDescriptor)
		if !argOK || !descOK || !bytes.Equal(leaseReq.Key, splitKey) {
			return nil
		}
		log.Infof(ctx, "received lease request (%s, %s)",
			leaseReq.Span(), leaseReq.Lease)
		if !reflect.DeepEqual(*forbiddenDesc, leaseReq.Lease.Replica) {
			return nil
		}
		log.Infof(ctx,
			"refusing lease request (%s, %s) because %+v held lease for LHS of split",
			leaseReq.Span(), leaseReq.Lease, forbiddenDesc)
		return kvpb.NewError(&kvpb.NotLeaseHolderError{RangeID: args.Hdr.RangeID})
	}

	var args base.TestClusterArgs
	args.ReplicationMode = base.ReplicationManual
	args.ServerArgs.Knobs.Store = &kvserver.StoreTestingKnobs{
		EvalKnobs: kvserverbase.BatchEvalTestingKnobs{
			TestingEvalFilter: filter,
		},
	}

	tc := testcluster.StartTestCluster(t, 2, args)
	defer tc.Stopper().Stop(context.Background())

	// Split the data range, mainly to avoid other splits getting in our way.
	for _, k := range []roachpb.Key{leftKey, rightKey} {
		if _, _, err := tc.SplitRange(k); err != nil {
			t.Fatal(errors.Wrapf(err, "split at %s", k))
		}
	}
	if _, err := tc.AddVoters(leftKey, tc.Target(1)); err != nil {
		t.Fatal(err)
	}

	db := tc.Servers[0].DB() // irrelevant which one we use

	// Make a context tied to the Stopper. The test works without, but this
	// is cleaner since we won't properly terminate the transaction below.
	ctx, cancel := tc.Server(0).Stopper().WithCancelOnQuiesce(ctx)
	defer cancel()

	// This transaction will try to write "under" a served read.
	txnOld := kv.NewTxn(ctx, db, 0 /* gatewayNodeID */)

	// Do something with txnOld so that its timestamp gets set.
	if _, err := txnOld.Scan(ctx, "a", "b", 0); err != nil {
		t.Fatal(err)
	}

	// Another client comes along at a higher timestamp, touching everything on
	// the right of the (soon-to-be) split key.
	if _, err := db.Scan(ctx, splitKey, rightKey, 0); err != nil {
		t.Fatal(err)
	}

	// This block makes sure that from now on, we don't allow the current
	// lease holder of our range to extend. Any attempt of doing so will
	// catch a NotLeaseHolderError, which means a retry by DistSender (until
	// the other node gets to be the lease holder instead).
	//
	// This makes sure that once we split, we'll be in the situation described
	// in #7899 (before the fix): The first lease holder of the right hand side
	// of the Split will not be that of the pre-split Range.
	// With the fix, the right-hand lease is initialized from the left-hand
	// lease, so the lease holders are the same, and there will never be a
	// lease request for the right-hand side in this test.
	leaseHolder := func(k roachpb.Key) roachpb.ReplicaDescriptor {
		desc, err := tc.LookupRange(k)
		if err != nil {
			t.Fatal(err)
		}
		lease, _, err := tc.FindRangeLease(desc, nil)
		if err != nil {
			t.Fatal(err)
		}
		leaseHolder := lease.Replica
		replica, found := desc.GetReplicaDescriptor(leaseHolder.StoreID)
		if !found {
			t.Fatalf("no replica on store %d found in %+v", leaseHolder.StoreID, desc)
		}
		return replica
	}
	blocklistedLeaseHolder := leaseHolder(leftKey)
	log.Infof(ctx, "blocklisting replica %+v for leases", blocklistedLeaseHolder)
	noLeaseForDesc.Store(&blocklistedLeaseHolder)

	// Pull the trigger. This actually also reads the RHS descriptor after the
	// split, so when this returns, we've got the leases set up already.
	//
	// There's a slight race here: Just above, we've settled on who must not
	// be the future lease holder. But between then and now, that lease could
	// have expired and the other Replica could have obtained it. This would
	// have given it a proper initialization of the timestamp cache, and so
	// the split trigger would populate the right hand side with a timestamp
	// cache which does not exhibit the anomaly.
	//
	// In practice, this should only be possible if second-long delays occur
	// just above this comment, and we assert against it below.
	log.Infof(ctx, "splitting at %s", splitKey)
	if _, _, err := tc.SplitRange(splitKey); err != nil {
		t.Fatal(err)
	}

	if currentLHSLeaseHolder := leaseHolder(leftKey); !reflect.DeepEqual(
		currentLHSLeaseHolder, blocklistedLeaseHolder) {
		t.Fatalf("lease holder changed from %+v to %+v, should de-flake this test",
			blocklistedLeaseHolder, currentLHSLeaseHolder)
	}

	// This write (to the right-hand side of the split) should hit the
	// timestamp cache and flag the txn for a restart when we try to commit it
	// below. With the bug in #7899, the RHS of the split had an empty
	// timestamp cache and would simply let us write behind the previous read.
	if err := txnOld.Put(ctx, "bb", "bump"); err != nil {
		t.Fatal(err)
	}

	if err := txnOld.Commit(ctx); err != nil {
		t.Fatalf("unexpected txn commit err: %+v", err)
	}

	// Verify that the txn's safe timestamp was set.
	if txnOld.TestingCloneTxn().ReadTimestamp.IsEmpty() {
		t.Fatal("expected non-zero refreshed timestamp")
	}

	// As outlined above, the anomaly was fixed by giving the right-hand side
	// of the split the same lease as the left-hand side of the Split. Check
	// that that's what's happened (we actually test a little more, namely
	// that it's the same ReplicaID, which is not required but should always
	// hold).
	if rhsLease := leaseHolder(rightKey); !reflect.DeepEqual(
		rhsLease, blocklistedLeaseHolder,
	) {
		t.Errorf("expected LHS and RHS to have same lease holder")
	}
}

// TestStoreSplitOnRemovedReplica prevents regression of #23673. In that issue,
// it was observed that the retry loop in AdminSplit could go into an infinite
// loop if the replica it was being run on had been removed from the range. The
// loop now checks that the replica performing the split is the leaseholder
// before each iteration.
func TestStoreSplitOnRemovedReplica(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	leftKey := roachpb.Key("a")
	splitKey := roachpb.Key("b")
	rightKey := roachpb.Key("c")

	var newDesc roachpb.RangeDescriptor
	inFilter := make(chan struct{}, 1)
	beginBlockingSplit := make(chan struct{})
	finishBlockingSplit := make(chan struct{})
	filter := func(_ context.Context, ba *kvpb.BatchRequest) *kvpb.Error {
		// Block replica 1's attempt to perform the AdminSplit. We detect the
		// split's range descriptor update and block until the rest of the test
		// is ready. We then return a ConditionFailedError, simulating a
		// descriptor update race.
		if ba.Replica.NodeID == 1 {
			for _, req := range ba.Requests {
				if cput, ok := req.GetInner().(*kvpb.ConditionalPutRequest); ok {
					leftDescKey := keys.RangeDescriptorKey(roachpb.RKey(leftKey))
					if cput.Key.Equal(leftDescKey) {
						var desc roachpb.RangeDescriptor
						if err := cput.Value.GetProto(&desc); err != nil {
							panic(err)
						}

						if desc.EndKey.Equal(splitKey) {
							select {
							case <-beginBlockingSplit:
								select {
								case inFilter <- struct{}{}:
									// Let the test know we're in the filter.
								default:
								}
								<-finishBlockingSplit

								var val roachpb.Value
								if err := val.SetProto(&newDesc); err != nil {
									panic(err)
								}
								return kvpb.NewError(&kvpb.ConditionFailedError{
									ActualValue: &val,
								})
							default:
							}
						}
					}
				}
			}
		}
		return nil
	}

	var args base.TestClusterArgs
	args.ReplicationMode = base.ReplicationManual
	args.ServerArgs.Knobs.Store = &kvserver.StoreTestingKnobs{
		TestingRequestFilter: filter,
	}

	tc := testcluster.StartTestCluster(t, 3, args)
	defer tc.Stopper().Stop(context.Background())

	// Split the data range, mainly to avoid other splits getting in our way.
	for _, k := range []roachpb.Key{leftKey, rightKey} {
		if _, _, err := tc.SplitRange(k); err != nil {
			t.Fatal(errors.Wrapf(err, "split at %s", k))
		}
	}

	// Send an AdminSplit request to the replica. In the filter above we'll
	// block the first cput in this split until we're ready to let it loose
	// again, which will be after we remove the replica from the range.
	splitRes := make(chan error)
	close(beginBlockingSplit)
	go func() {
		_, _, err := tc.SplitRange(splitKey)
		splitRes <- err
	}()
	<-inFilter

	// Move the range from node 0 to node 1. Then add node 2 to the range.
	// node 0 will never hear about this range descriptor update.
	var err error
	if newDesc, err = tc.AddVoters(leftKey, tc.Target(1)); err != nil {
		t.Fatal(err)
	}
	if err := tc.TransferRangeLease(newDesc, tc.Target(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.RemoveVoters(leftKey, tc.Target(0)); err != nil {
		t.Fatal(err)
	}
	if newDesc, err = tc.AddVoters(leftKey, tc.Target(2)); err != nil {
		t.Fatal(err)
	}

	// Stop blocking the split request's cput. This will cause the cput to fail
	// with a ConditionFailedError. The error will warrant a retry in
	// AdminSplit's retry loop, but when the removed replica notices that it is
	// no longer the leaseholder, it will return a NotLeaseholderError. This in
	// turn will allow the AdminSplit to be re-routed to the new leaseholder,
	// where it will succeed.
	close(finishBlockingSplit)
	if err = <-splitRes; err != nil {
		t.Errorf("AdminSplit returned error: %+v", err)
	}
}

func TestStoreSplitGCThreshold(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	leftKey := roachpb.Key("a")
	splitKey := roachpb.Key("b")
	rightKey := roachpb.Key("c")
	content := []byte("test")

	pArgs := putArgs(leftKey, content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs(rightKey, content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	specifiedGCThreshold := hlc.Timestamp{
		WallTime: 2e9,
	}
	gcArgs := &kvpb.GCRequest{
		RequestHeader: kvpb.RequestHeader{
			Key:    leftKey,
			EndKey: rightKey,
		},
		Threshold: specifiedGCThreshold,
	}
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), gcArgs); pErr != nil {
		t.Fatal(pErr)
	}

	args := adminSplitArgs(splitKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	repl := store.LookupReplica(roachpb.RKey(splitKey))
	gcThreshold := repl.GetGCThreshold()

	if !reflect.DeepEqual(gcThreshold, specifiedGCThreshold) {
		t.Fatalf("expected RHS's GCThreshold is equal to %v, but got %v", specifiedGCThreshold, gcThreshold)
	}

	repl.AssertState(ctx, store.TODOEngine())
}

func TestStoreSplitGCHint(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	leftKey := roachpb.Key("a")
	splitKey := roachpb.Key("b")
	rightKey := roachpb.Key("c")
	content := []byte("test")

	pArgs := putArgs(leftKey, content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs = putArgs(rightKey, content)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	r, err := s.LookupRange(splitKey)
	require.NoError(t, err, "failed to lookup range")

	repl := store.LookupReplica(roachpb.RKey(splitKey))
	gcHint := repl.GetGCHint()
	require.True(t, gcHint.IsEmpty(), "GC hint is not empty by default")

	drArgs := &kvpb.DeleteRangeRequest{
		UpdateRangeDeleteGCHint: true,
		UseRangeTombstone:       true,
		RequestHeader: kvpb.RequestHeader{
			Key:    r.StartKey.AsRawKey(),
			EndKey: r.EndKey.AsRawKey(),
		},
	}
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), drArgs); pErr != nil {
		t.Fatal(pErr)
	}

	args := adminSplitArgs(splitKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	gcHint = repl.GetGCHint()
	require.False(t, gcHint.IsEmpty(), "GC hint is empty after range delete")

	repl.AssertState(ctx, store.TODOEngine())
}

// TestStoreRangeSplitRaceUninitializedRHS reproduces #7600 (before it was
// fixed). While splits are happening, we simulate incoming messages for the
// right-hand side to trigger a race between the creation of the proper replica
// and the uninitialized replica reacting to messages.
func TestStoreRangeSplitRaceUninitializedRHS(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// The aggressive Raft timeouts in this test prevents it from maintaining
	// quorum and making progress when stressing under race or deadlock detection.
	skip.UnderRace(t)
	skip.UnderDeadlock(t)

	currentTrigger := make(chan *roachpb.SplitTrigger, 1)
	var seen struct {
		syncutil.Mutex
		sids map[kvserverbase.CmdIDKey][2]bool
	}
	seen.sids = make(map[kvserverbase.CmdIDKey][2]bool)

	testingEvalFilter := func(args kvserverbase.FilterArgs) *kvpb.Error {
		et, ok := args.Req.(*kvpb.EndTxnRequest)
		if !ok || et.InternalCommitTrigger == nil {
			return nil
		}
		trigger := protoutil.Clone(et.InternalCommitTrigger.GetSplitTrigger()).(*roachpb.SplitTrigger)
		// The first time the trigger arrives (on each of the two stores),
		// return a transaction retry. This allows us to pass the trigger to
		// the goroutine creating faux incoming messages for the yet
		// nonexistent right-hand-side, giving it a head start. This code looks
		// fairly complicated since it wants to ensure that the two replicas
		// don't diverge.
		if trigger != nil && len(trigger.RightDesc.InternalReplicas) == 2 && args.Hdr.Txn.Epoch == 0 {
			seen.Lock()
			defer seen.Unlock()
			sid, sl := int(args.Sid)-1, seen.sids[args.CmdID]
			if !sl[sid] {
				sl[sid] = true
				seen.sids[args.CmdID] = sl
			} else {
				return nil
			}
			select {
			case currentTrigger <- trigger:
			default:
			}
			return kvpb.NewError(
				kvpb.NewReadWithinUncertaintyIntervalError(
					args.Hdr.Timestamp, hlc.ClockTimestamp{}, nil, args.Hdr.Timestamp, hlc.ClockTimestamp{},
				))
		}
		return nil
	}

	ctx := context.Background()
	tc := testcluster.StartTestCluster(t, 2,
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				Knobs: base.TestingKnobs{
					Store: &kvserver.StoreTestingKnobs{
						EvalKnobs: kvserverbase.BatchEvalTestingKnobs{
							TestingEvalFilter: testingEvalFilter,
						},
					},
				},
				RaftConfig: base.RaftConfig{
					// An aggressive tick interval lets groups communicate more and thus
					// triggers test failures much more reliably. We can't go too aggressive
					// or race tests never make any progress.
					RaftTickInterval:           100 * time.Millisecond,
					RaftElectionTimeoutTicks:   2,
					RaftHeartbeatIntervalTicks: 1,
				},
			},
		})
	defer tc.Stopper().Stop(ctx)
	store := tc.GetFirstStoreFromServer(t, 0)

	leftRange := store.LookupReplica(roachpb.RKey("a"))

	// Replicate the left range onto the second node. We don't wait since we
	// don't actually care what the second node does. All we want is that the
	// first node isn't surprised by messages from that node.
	tc.AddVotersOrFatal(t, leftRange.Desc().StartKey.AsRawKey(), tc.Target(1))

	for i := 0; i < 10; i++ {
		errChan := make(chan *kvpb.Error)
		failedSendLog := log.Every(time.Second)

		// Closed when the split goroutine is done.
		splitDone := make(chan struct{})

		go func(i int) {
			defer close(splitDone)

			// Split the data range. The split keys are chosen so that they move
			// towards "a" (so that the range being split is always the first
			// range).
			splitKey := roachpb.Key(encoding.EncodeVarintDescending([]byte("a"), int64(i)))
			splitArgs := adminSplitArgs(splitKey)
			_, pErr := kv.SendWrapped(context.Background(), tc.Servers[0].DistSenderI().(kv.Sender), splitArgs)
			errChan <- pErr
		}(i)
		go func() {
			defer func() { errChan <- nil }()

			trigger := <-currentTrigger // our own copy
			// Make sure the first node is first for convenience.
			replicas := trigger.RightDesc.InternalReplicas
			if replicas[0].NodeID > replicas[1].NodeID {
				tmp := replicas[1]
				replicas[1] = replicas[0]
				replicas[0] = tmp
			}

			// Send a few vote requests which look like they're from the other
			// node's right hand side of the split. This triggers a race which
			// is discussed in #7600 (briefly, the creation of the right hand
			// side in the split trigger was racing with the uninitialized
			// version for the same group, resulting in clobbered HardState).
			for term := uint64(1); ; term++ {
				if sent := tc.Servers[1].RaftTransport().(*kvserver.RaftTransport).SendAsync(&kvserverpb.RaftMessageRequest{
					RangeID:     trigger.RightDesc.RangeID,
					ToReplica:   replicas[0],
					FromReplica: replicas[1],
					Message: raftpb.Message{
						Type: raftpb.MsgVote,
						To:   raftpb.PeerID(replicas[0].ReplicaID),
						From: raftpb.PeerID(replicas[1].ReplicaID),
						Term: term,
					},
				}, rpcbase.DefaultClass); !sent {
					// SendAsync can return false, indicating the message didn't send.
					// The most likely reason this test encounters a message failing to
					// send is the outgoing message queue being full. The queue filling
					// up is expected given it has fixed capacity and this loop is
					// attempting to sending 1 MsgVote every microsecond. See comments
					// below and above for the frequency rationale.
					if failedSendLog.ShouldLog() {
						log.Infof(ctx, "transport failed to send vote request")
					}
				}
				select {
				case <-splitDone:
					return
				case <-time.After(time.Microsecond):
					// If we busy-loop here, we monopolize processRaftMu and the
					// split takes a long time to complete. Sleeping reduces the
					// chance that we hit the race, but it still shows up under
					// stress.
				}
			}
		}()
		for i := 0; i < 2; i++ {
			if pErr := <-errChan; pErr != nil {
				t.Fatal(pErr)
			}
		}
	}
}

// TestLeaderAfterSplit verifies that a raft group created by a split elects a
// leader without waiting for an election timeout. It also tests that we don't
// get an election tie, because we only allow the leaseholder to campaign if
// there is a valid lease.
func TestLeaderAfterSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Timing-sensitive test, disable under deadlock and race.
	skip.UnderDeadlock(t)
	skip.UnderRace(t)

	testutils.RunValues(t, "lease-type", roachpb.TestingAllLeaseTypes(), func(t *testing.T, leaseType roachpb.LeaseType) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // time out early
		defer cancel()

		settings := cluster.MakeTestingClusterSettings()
		kvserver.OverrideDefaultLeaseType(ctx, &settings.SV, leaseType)
		raftConfig := base.RaftConfig{
			RangeLeaseDuration: time.Hour, // don't expire leases
		}
		// Suppress timeout-based elections to avoid leadership changes in ways this
		// test doesn't expect. For leader leases, fortification itself provides us
		// this guarantee.
		if leaseType != roachpb.LeaseLeader {
			raftConfig.RaftElectionTimeoutTicks = 1000000
		}
		tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				Settings:   settings,
				RaftConfig: raftConfig,
			},
		})
		defer tc.Stopper().Stop(ctx)

		store := tc.GetFirstStoreFromServer(t, 0)
		sender := tc.Servers[0].DistSenderI().(kv.Sender)

		leftKey := roachpb.Key("a")
		splitKey := roachpb.Key("m")
		rightKey := roachpb.Key("z")

		repl := store.LookupReplica(roachpb.RKey(leftKey))
		require.NotNil(t, repl)
		tc.AddVotersOrFatal(t, repl.Desc().StartKey.AsRawKey(), tc.Targets(1, 2)...)

		// Split the range.
		_, pErr := kv.SendWrapped(ctx, sender, adminSplitArgs(splitKey))
		require.NoError(t, pErr.GoError())

		// Make sure both the LHS and RHS can replicate a write request. This will
		// time out if the RHS can't elect a Raft leader.
		_, pErr = kv.SendWrapped(ctx, sender, incrementArgs(leftKey, 1))
		require.NoError(t, pErr.GoError())

		_, pErr = kv.SendWrapped(ctx, sender, incrementArgs(rightKey, 1))
		require.NoError(t, pErr.GoError())
	})
}

func BenchmarkStoreRangeSplit(b *testing.B) {
	defer log.Scope(b).Close(b)

	ctx := context.Background()
	s := serverutils.StartServerOnly(b, base.TestServerArgs{})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(b, err)

	_, err = s.ScratchRange()
	require.NoError(b, err)

	// Perform initial split of ranges.
	sArgs := adminSplitArgs(scratchKey("b"))
	if _, err := kv.SendWrapped(ctx, store.TestSender(), sArgs); err != nil {
		b.Fatal(err)
	}

	// Write some values left and right of the split key.
	aDesc := store.LookupReplica([]byte(scratchKey("a"))).Desc()
	bDesc := store.LookupReplica([]byte(scratchKey("c"))).Desc()
	kvserver.WriteRandomDataToRange(b, store, aDesc.RangeID, scratchKey("aaa"))
	kvserver.WriteRandomDataToRange(b, store, bDesc.RangeID, scratchKey("ccc"))

	// Merge the b range back into the a range.
	mArgs := adminMergeArgs(keys.ScratchRangeMin)
	if _, err := kv.SendWrapped(ctx, store.TestSender(), mArgs); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Split the range.
		b.StartTimer()
		if _, err := kv.SendWrapped(ctx, store.TestSender(), sArgs); err != nil {
			b.Fatal(err)
		}

		// Merge the ranges.
		b.StopTimer()
		if _, err := kv.SendWrapped(ctx, store.TestSender(), mArgs); err != nil {
			b.Fatal(err)
		}
	}
}

func writeRandomTimeSeriesDataToRange(
	t testing.TB, store *kvserver.Store, rangeID roachpb.RangeID, keyPrefix []byte,
) (midpoint []byte) {
	src := rand.New(rand.NewSource(0))
	r := ts.Resolution10s
	for i := 0; i < 20; i++ {
		var data []tspb.TimeSeriesData
		for j := int64(0); j <= src.Int63n(5); j++ {
			d := tspb.TimeSeriesData{
				Name:   "test.random.metric",
				Source: "cpu01",
			}
			for k := int64(0); k <= src.Int63n(10); k++ {
				d.Datapoints = append(d.Datapoints, tspb.TimeSeriesDatapoint{
					TimestampNanos: src.Int63n(200) * r.SlabDuration(),
					Value:          src.Float64(),
				})
			}
			data = append(data, d)
		}
		for _, d := range data {
			idatas, err := d.ToInternal(r.SlabDuration(), r.SampleDuration(), false)
			if err != nil {
				t.Fatal(err)
			}
			for _, idata := range idatas {
				var value roachpb.Value
				if err := value.SetProto(&idata); err != nil {
					t.Fatal(err)
				}
				mArgs := kvpb.MergeRequest{
					RequestHeader: kvpb.RequestHeader{
						Key: encoding.EncodeVarintAscending(keyPrefix, idata.StartTimestampNanos),
					},
					Value: value,
				}
				if _, pErr := kv.SendWrappedWith(context.Background(), store.TestSender(), kvpb.Header{
					RangeID: rangeID,
				}, &mArgs); pErr != nil {
					t.Fatal(pErr)
				}
			}
		}
	}
	// Return approximate midway point (100 is midway between random timestamps in range [0,200)).
	midKey := append([]byte(nil), keyPrefix...)
	midKey = encoding.EncodeVarintAscending(midKey, 100*r.SlabDuration())
	return midKey
}

// TestStoreRangeGossipOnSplits verifies that the store descriptor
// is gossiped on splits up until the point where an additional
// split range doesn't exceed GossipWhenCapacityDeltaExceedsFraction.
func TestStoreRangeGossipOnSplits(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// 50% for testing We can't properly test how frequently changes in the
	// number of ranges trigger the store to gossip its capacities if we have
	// to worry about changes in the number of leases also triggering store
	// gossip.
	overrideCapacityFraction := 0.5

	ctx := context.Background()

	// Override the store gossip frequency to zero, so that gossip always
	// triggers on capacity changes.
	st := cluster.MakeTestingClusterSettings()
	kvserver.MaxStoreGossipFrequency.Override(ctx, &st.SV, 0*time.Millisecond)
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Settings: st,
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
				DisableScanner:    true,
				GossipTestingKnobs: kvserver.StoreGossipTestingKnobs{
					OverrideGossipWhenCapacityDeltaExceedsFraction: overrideCapacityFraction,
					DisableLeaseCapacityGossip:                     true,
				},
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)
	storeKey := gossip.MakeStoreDescKey(store.StoreID())

	// Avoid excessive logging on under-replicated ranges due to our many splits.
	config.TestingSetupZoneConfigHook(s.Stopper())
	zoneConfig := zonepb.DefaultZoneConfig()
	zoneConfig.NumReplicas = proto.Int32(1)
	config.TestingSetZoneConfig(0, zoneConfig)

	var lastSD roachpb.StoreDescriptor
	rangeCountCh := make(chan int32)
	unregister := store.Gossip().RegisterCallback(storeKey, func(_ string, val roachpb.Value, _ int64) {
		var sd roachpb.StoreDescriptor
		if err := val.GetProto(&sd); err != nil {
			panic(err)
		}
		// Wait for range count to change as this callback is invoked
		// for lease count changes as well.
		if sd.Capacity.RangeCount == lastSD.Capacity.RangeCount {
			return
		}
		lastSD = sd
		rangeCountCh <- sd.Capacity.RangeCount
	})
	defer unregister()

	// Pull the first gossiped range count.
	lastRangeCount := <-rangeCountCh

	splitFunc := func(i int) *kvpb.Error {
		splitKey := roachpb.Key(fmt.Sprintf("%02d", i))
		_, pErr := store.LookupReplica(roachpb.RKey(splitKey)).AdminSplit(
			context.Background(),
			kvpb.AdminSplitRequest{
				RequestHeader: kvpb.RequestHeader{
					Key: splitKey,
				},
				SplitKey: splitKey,
			},
			"test",
		)
		return pErr
	}

	// Split until we split at least 20 ranges.
	var rangeCount int32
	for i := 0; rangeCount < 20; i++ {
		if pErr := splitFunc(i); pErr != nil {
			// Avoid flakes caused by bad clocks.
			if testutils.IsPError(pErr, "rejecting command with timestamp in the future") {
				log.Warningf(context.Background(), "ignoring split error: %s", pErr)
				continue
			}
			t.Fatal(pErr)
		}
		select {
		case rangeCount = <-rangeCountCh:
			changeCount := int32(math.Ceil(math.Max(
				float64(lastRangeCount)*overrideCapacityFraction,
				kvserver.GossipWhenRangeCountDeltaExceeds,
			)))
			diff := rangeCount - (lastRangeCount + changeCount)
			if diff < -1 || diff > 1 {
				t.Errorf("gossiped range count %d more than 1 away from expected %d", rangeCount, lastRangeCount+changeCount)
			}
			lastRangeCount = rangeCount
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestStoreTxnWaitQueueEnabledOnSplit verifies that the TxnWaitQueue for
// the right hand side of the split range is enabled after a split.
func TestStoreTxnWaitQueueEnabledOnSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	key := bootstrap.TestingUserTableDataMin(keys.SystemSQLCodec)
	args := adminSplitArgs(key)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatalf("%q: split unexpected error: %s", key, pErr)
	}

	rhsRepl := store.LookupReplica(roachpb.RKey(bootstrap.TestingUserTableDataMin(keys.SystemSQLCodec)))
	if !rhsRepl.GetConcurrencyManager().TestingTxnWaitQueue().IsEnabled() {
		t.Errorf("expected RHS replica's push txn queue to be enabled post-split")
	}
}

// TestDistributedTxnCleanup verifies that distributed transactions
// cleanup their txn records after commit or abort.
func TestDistributedTxnCleanup(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Split at "a".
	lhsKey := roachpb.Key("a")
	args := adminSplitArgs(lhsKey)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatalf("split at %q: %s", lhsKey, pErr)
	}
	lhs := store.LookupReplica(roachpb.RKey("a"))

	// Split at "b".
	rhsKey := roachpb.Key("b")
	args = adminSplitArgs(rhsKey)
	if _, pErr := kv.SendWrappedWith(context.Background(), store, kvpb.Header{
		RangeID: lhs.RangeID,
	}, args); pErr != nil {
		t.Fatalf("split at %q: %s", rhsKey, pErr)
	}
	rhs := store.LookupReplica(roachpb.RKey("b"))

	if lhs == rhs {
		t.Errorf("LHS == RHS after split: %s == %s", lhs, rhs)
	}

	// Test both commit and abort cases.
	testutils.RunTrueAndFalse(t, "force", func(t *testing.T, force bool) {
		testutils.RunTrueAndFalse(t, "commit", func(t *testing.T, commit bool) {
			// Run a distributed transaction involving the lhsKey and rhsKey.
			var txnKey roachpb.Key
			txn := kv.NewTxn(ctx, store.DB(), 0 /* gatewayNodeID */)
			txnFn := func(ctx context.Context, txn *kv.Txn) error {
				b := txn.NewBatch()
				b.Put(fmt.Sprintf("%s.force=%t,commit=%t", string(lhsKey), force, commit), "lhsValue")
				b.Put(fmt.Sprintf("%s.force=%t,commit=%t", string(rhsKey), force, commit), "rhsValue")
				if err := txn.Run(ctx, b); err != nil {
					return err
				}
				proto := txn.TestingCloneTxn()
				txnKey = keys.TransactionKey(proto.Key, proto.ID)
				// If force=true, we're force-aborting the txn out from underneath.
				// This simulates txn deadlock or a max priority txn aborting a
				// normal or min priority txn.
				if force {
					ba := &kvpb.BatchRequest{}
					ba.Timestamp = store.Clock().Now()
					ba.RangeID = lhs.RangeID
					ba.Add(&kvpb.PushTxnRequest{
						RequestHeader: kvpb.RequestHeader{
							Key: proto.Key,
						},
						PusheeTxn: proto.TxnMeta,
						PushType:  kvpb.PUSH_ABORT,
						Force:     true,
					})
					_, pErr := store.Send(ctx, ba)
					if pErr != nil {
						t.Fatalf("failed to abort the txn: %s", pErr)
					}
				}
				if commit {
					return txn.Commit(ctx)
				}
				return errors.New("forced abort")
			}
			if err := txnFn(ctx, txn); err != nil {
				require.NoError(t, txn.Rollback(ctx))
				if !force && commit {
					t.Fatalf("expected success with commit == true; got %v", err)
				}
			}

			// Verify that the transaction record is cleaned up.
			testutils.SucceedsSoon(t, func() error {
				kv, err := store.DB().Get(ctx, txnKey)
				if err != nil {
					return err
				}
				if kv.Value != nil {
					return errors.Errorf("expected txn record %s to have been cleaned", txnKey)
				}
				return nil
			})
		})
	})
}

// TestUnsplittableRange creates an unsplittable range and tests that
// it is handled correctly by the split queue's purgatory. The test:
// 1. creates an unsplittable range that needs to be split
// 2. makes sure that range enters purgatory
// 3. makes sure a purgatory run still fails
// 4. GCs part of the range so that it no longer needs to be split
// 5. makes sure a purgatory run succeeds and the range leaves purgatory
func TestUnsplittableRange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	ttl := 1 * time.Hour
	defer zonepb.TestingSetMinRangeMaxBytes(1 << 16)()
	const minBytes = 1 << 12
	const maxBytes = 1 << 18
	manualClock := hlc.NewHybridManualClock()
	zoneConfig := zonepb.DefaultZoneConfig()
	zoneConfig.RangeMinBytes = proto.Int64(minBytes)
	zoneConfig.RangeMaxBytes = proto.Int64(maxBytes)
	zoneConfig.GC = &zonepb.GCPolicy{
		TTLSeconds: int32(ttl.Seconds()),
	}
	zoneConfig.NumReplicas = proto.Int32(1)
	zoneSystemConfig := zonepb.DefaultSystemZoneConfig()
	zoneSystemConfig.RangeMinBytes = proto.Int64(minBytes)
	zoneSystemConfig.RangeMaxBytes = proto.Int64(maxBytes)
	zoneSystemConfig.GC = &zonepb.GCPolicy{
		TTLSeconds: int32(ttl.Seconds()),
	}
	zoneSystemConfig.NumReplicas = proto.Int32(1)
	splitQueuePurgatoryChan := make(chan time.Time, 1)

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:       true,
				SplitQueuePurgatoryChan: splitQueuePurgatoryChan,
				// Without this, the test is flaky: the range does not end up in
				// purgatory "synchronously".
				DisableCanAckBeforeApplication: true,
			},
			Server: &server.TestingKnobs{
				WallClock:                       manualClock,
				DefaultZoneConfigOverride:       &zoneConfig,
				DefaultSystemZoneConfigOverride: &zoneSystemConfig,
			},
			SpanConfig: &spanconfig.TestingKnobs{
				ProtectedTSReaderOverrideFn: spanconfig.EmptyProtectedTSReader,
			},
			KeyVisualizer:  &keyvisualizer.TestingKnobs{SkipZoneConfigBootstrap: true},
			UpgradeManager: &upgradebase.TestingKnobs{SkipZoneConfigBootstrap: true},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Add a single large row to /Table/14.
	tableKey := roachpb.RKey(keys.SystemSQLCodec.TablePrefix(uint32(systemschema.UITable.GetID())))
	row1Key := roachpb.Key(encoding.EncodeVarintAscending(append([]byte(nil), tableKey...), 1))
	col1Key := keys.MakeFamilyKey(append([]byte(nil), row1Key...), 0)
	valueLen := 0.9 * maxBytes
	value := bytes.Repeat([]byte("x"), int(valueLen))
	if err := store.DB().Put(ctx, col1Key, value); err != nil {
		t.Fatal(err)
	}

	// Wait for half of the ttl and add another large value in the same row.
	// Together, these two values bump the range over the max range size.
	manualClock.Increment(ttl.Nanoseconds() / 2)
	value2Len := 0.2 * maxBytes
	value2 := bytes.Repeat([]byte("y"), int(value2Len))
	if err := store.DB().Put(ctx, col1Key, value2); err != nil {
		t.Fatal(err)
	}
	// Ensure that an attempt to split the range will hit an
	// unsplittableRangeError and place the range in purgatory.
	if err := store.ForceSplitScanAndProcess(); err != nil {
		t.Fatal(err)
	}
	if purgLen := store.SplitQueuePurgatoryLength(); purgLen != 1 {
		t.Fatalf("expected split queue purgatory to contain 1 replica, found %d", purgLen)
	}

	// Signal the split queue's purgatory channel and ensure that the purgatory
	// remains occupied because the range still needs to split but can't.
	splitQueuePurgatoryChan <- timeutil.Now()
	if purgLen := store.SplitQueuePurgatoryLength(); purgLen != 1 {
		t.Fatalf("expected split queue purgatory to contain 1 replica, found %d", purgLen)
	}

	// Wait for much longer than the ttl to accumulate GCByteAge.
	manualClock.Increment(10 * ttl.Nanoseconds())
	// Trigger the MVCC GC queue, which should clean up the earlier version of the
	// row. Once the first version of the row is cleaned up, the range should
	// exit the split queue purgatory.
	repl := store.LookupReplica(tableKey)
	if err := store.ManualMVCCGC(repl); err != nil {
		t.Fatal(err)
	}

	// Signal the split queue's purgatory channel and ensure that the purgatory
	// removes its now well-sized replica.
	splitQueuePurgatoryChan <- timeutil.Now()
	testutils.SucceedsSoon(t, func() error {
		purgLen := store.SplitQueuePurgatoryLength()
		if purgLen == 0 {
			return nil
		}
		return errors.Errorf("expected split queue purgatory to be empty, found %d", purgLen)
	})
}

// TestTxnWaitQueueDependencyCycleWithRangeSplit verifies that a range
// split which occurs while a dependency cycle is partially underway
// will cause the pending push txns to be retried such that they
// relocate to the appropriate new range.
func TestTxnWaitQueueDependencyCycleWithRangeSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testutils.RunTrueAndFalse(t, "read2ndPass", func(t *testing.T, read2ndPass bool) {
		var pushCount int32
		firstPush := make(chan struct{})

		testingEvalFilter :=
			func(filterArgs kvserverbase.FilterArgs) *kvpb.Error {
				if _, ok := filterArgs.Req.(*kvpb.PushTxnRequest); ok {
					if atomic.AddInt32(&pushCount, 1) == 1 {
						close(firstPush)
					}
				}
				return nil
			}
		ctx := context.Background()
		s := serverutils.StartServerOnly(t, base.TestServerArgs{
			Knobs: base.TestingKnobs{
				Store: &kvserver.StoreTestingKnobs{
					DisableMergeQueue: true,
					DisableSplitQueue: true,
					EvalKnobs: kvserverbase.BatchEvalTestingKnobs{
						TestingEvalFilter: testingEvalFilter,
					},
				},
			},
		})

		defer s.Stopper().Stop(ctx)
		store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
		require.NoError(t, err)

		lhsKey := roachpb.Key("a")
		rhsKey := roachpb.Key("b")

		// Split at "a".
		args := adminSplitArgs(lhsKey)
		if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
			t.Fatalf("split at %q: %s", lhsKey, pErr)
		}
		lhs := store.LookupReplica(roachpb.RKey("a"))

		var txnACount, txnBCount int32

		txnAWritesA := make(chan struct{})
		txnAProceeds := make(chan struct{})
		txnBWritesB := make(chan struct{})
		txnBProceeds := make(chan struct{})

		// Start txn to write key a.
		txnACh := make(chan error)
		go func() {
			txnACh <- store.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
				if err := txn.Put(ctx, lhsKey, "value"); err != nil {
					return err
				}
				if atomic.LoadInt32(&txnACount) == 0 {
					close(txnAWritesA)
					<-txnAProceeds
				}
				atomic.AddInt32(&txnACount, 1)
				return txn.Put(ctx, rhsKey, "value-from-A")
			})
		}()
		<-txnAWritesA

		// Start txn to write key b.
		txnBCh := make(chan error)
		go func() {
			txnBCh <- store.DB().Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
				if err := txn.Put(ctx, rhsKey, "value"); err != nil {
					return err
				}
				if atomic.LoadInt32(&txnBCount) == 0 {
					close(txnBWritesB)
					<-txnBProceeds
				}
				atomic.AddInt32(&txnBCount, 1)
				// Read instead of write key "a" if directed. This caused a
				// PUSH_TIMESTAMP to be issued from txn B instead of PUSH_ABORT.
				if read2ndPass {
					if _, err := txn.Get(ctx, lhsKey); err != nil {
						return err
					}
				} else {
					if err := txn.Put(ctx, lhsKey, "value-from-B"); err != nil {
						return err
					}
				}
				return nil
			})
		}()
		<-txnBWritesB

		// Now, let txnA proceed before splitting.
		close(txnAProceeds)
		// Wait for the push to occur.
		<-firstPush

		// Split at "b".
		args = adminSplitArgs(rhsKey)
		if _, pErr := kv.SendWrappedWith(ctx, store, kvpb.Header{
			RangeID: lhs.RangeID,
		}, args); pErr != nil {
			t.Fatalf("split at %q: %s", rhsKey, pErr)
		}

		// Now that we've split, allow txnB to proceed.
		close(txnBProceeds)

		// Verify that both complete.
		for i, ch := range []chan error{txnACh, txnBCh} {
			if err := <-ch; err != nil {
				t.Fatalf("%d: txn failure: %+v", i, err)
			}
		}
	})
}

func TestStoreCapacityAfterSplit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	kvserver.ExpirationLeasesOnly.Override(ctx, &st.SV, false) // override metamorphism
	manualClock := hlc.NewHybridManualClock()
	tc := testcluster.StartTestCluster(t, 2,
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				Settings: st,
				RaftConfig: base.RaftConfig{
					// We plan to increment the manual clock by MinStatsDuration a few
					// times below and would like for leases to not expire. Configure a
					// longer lease duration to achieve this.
					RangeLeaseDuration: 10 * replicastats.MinStatsDuration,
				},
				Knobs: base.TestingKnobs{
					Server: &server.TestingKnobs{
						WallClock: manualClock,
					},
					Store: &kvserver.StoreTestingKnobs{
						DisableCanAckBeforeApplication: true,
					},
				},
			},
		})
	defer tc.Stopper().Stop(ctx)
	// We conduct the test on the second server, because we can keep it clean
	// and control exactly which ranges end up on it.
	s := tc.GetFirstStoreFromServer(t, 1)
	key := tc.ScratchRange(t)
	desc := tc.AddVotersOrFatal(t, key, tc.Target(1))
	tc.TransferRangeLeaseOrFatal(t, desc, tc.Target(1))

	// Wait for the lease transfer to be applied on the new leaseholder and then
	// to be upgraded from an expiration-based lease.
	testutils.SucceedsSoon(t, func() error {
		repl, err := s.GetReplica(desc.RangeID)
		if err != nil {
			return err
		}
		l, _ := repl.GetLease()
		if !l.OwnedBy(s.StoreID()) {
			return errors.Errorf("lease transfer not applied on leaseholder")
		}
		if l.Type() == roachpb.LeaseExpiration {
			return errors.Errorf("lease still an expiration based lease")
		}
		return nil
	})

	cap, err := s.Capacity(ctx, false /* useCached */)
	if err != nil {
		t.Fatal(err)
	}
	if e, a := int32(1), cap.RangeCount; e != a {
		t.Errorf("expected cap.RangeCount=%d, got %d", e, a)
	}
	bpr1 := cap.BytesPerReplica
	if bpr1.P10 != 0 {
		t.Errorf("expected all bytes-per-replica to be 0, got %+v", bpr1)
	}
	if bpr1.P10 != bpr1.P25 || bpr1.P10 != bpr1.P50 || bpr1.P10 != bpr1.P75 || bpr1.P10 != bpr1.P90 {
		t.Errorf("expected all bytes-per-replica percentiles to be identical, got %+v", bpr1)
	}
	wpr1 := cap.WritesPerReplica
	if wpr1.P10 != wpr1.P25 || wpr1.P10 != wpr1.P50 || wpr1.P10 != wpr1.P75 || wpr1.P10 != wpr1.P90 {
		t.Errorf("expected all writes-per-replica percentiles to be identical, got %+v", wpr1)
	}

	// Increment the manual clock and do a write to increase the qps above zero.
	manualClock.Increment(int64(replicastats.MinStatsDuration))
	pArgs := incrementArgs(key, 10)
	if _, pErr := kv.SendWrapped(ctx, s.TestSender(), pArgs); pErr != nil {
		t.Fatal(pErr)
	}
	// We want to make sure we can read the value through raft, so we know
	// the stats are updated.
	testutils.SucceedsSoon(t, func() error {
		getArgs := getArgs(key)
		if reply, err := kv.SendWrapped(ctx, s.TestSender(), getArgs); err != nil {
			return errors.Errorf("failed to read data: %s", err)
		} else if e, v := int64(10), mustGetInt(reply.(*kvpb.GetResponse).Value); v != e {
			return errors.Errorf("failed to read correct data: expected %d, got %d", e, v)
		}
		return nil
	})

	// Bump the clock again, right before calling capacity. We know that the
	// writes have succeeded and should be reflected in Capacity, however the
	// MinStatsDuration will cause nothing to be returned unless the last lease
	// transfer is at least MinStatsDuration ago.
	manualClock.Increment(int64(replicastats.MinStatsDuration))
	cap, err = s.Capacity(ctx, false /* useCached */)
	if err != nil {
		t.Fatal(err)
	}
	if e, a := int32(1), cap.RangeCount; e != a {
		t.Errorf("expected cap.RangeCount=%d, got %d", e, a)
	}
	if e, a := int32(1), cap.LeaseCount; e != a {
		t.Errorf("expected cap.LeaseCount=%d, got %d", e, a)
	}

	// NB: The writes per second may be within some error bound below the
	// minExpected due to timing and floating point calculation. An error of
	// 0.01 (WPS) is added to avoid flaking the test.
	if minExpected, a := 1/(float64(2*replicastats.MinStatsDuration/time.Second)), cap.WritesPerSecond; minExpected > a+0.01 {
		t.Errorf("expected cap.WritesPerSecond >= %f, got %f", minExpected, a)
	}

	bpr2 := cap.BytesPerReplica
	if bpr2.P10 <= bpr1.P10 {
		t.Errorf("expected BytesPerReplica to have increased from %+v, but got %+v", bpr1, bpr2)
	}
	if bpr2.P10 != bpr2.P25 || bpr2.P10 != bpr2.P50 || bpr2.P10 != bpr2.P75 || bpr2.P10 != bpr2.P90 {
		t.Errorf("expected all bytes-per-replica percentiles to be identical, got %+v", bpr2)
	}
	wpr2 := cap.WritesPerReplica
	if wpr2.P10 <= wpr1.P10 {
		t.Errorf("expected WritesPerReplica to have increased from %+v, but got %+v", wpr1, wpr2)
	}
	if wpr2.P10 != wpr2.P25 || wpr2.P10 != wpr2.P50 || wpr2.P10 != wpr2.P75 || wpr2.P10 != wpr2.P90 {
		t.Errorf("expected all writes-per-replica percentiles to be identical, got %+v", wpr2)
	}
	if wpr2.P10 != cap.WritesPerSecond {
		t.Errorf("expected WritesPerReplica.percentiles to equal cap.WritesPerSecond, but got %f and %f",
			wpr2.P10, cap.WritesPerSecond)
	}

	// Split the range to verify stats work properly with more than one range.
	sArgs := adminSplitArgs(key.Next().Next())
	if _, pErr := kv.SendWrapped(ctx, s.TestSender(), sArgs); pErr != nil {
		t.Fatal(pErr)
	}

	cap, err = s.Capacity(ctx, false /* useCached */)
	if err != nil {
		t.Fatal(err)
	}
	if e, a := int32(2), cap.RangeCount; e != a {
		t.Errorf("expected cap.RangeCount=%d, got %d", e, a)
	}
	if e, a := int32(2), cap.LeaseCount; e != a {
		t.Errorf("expected cap.LeaseCount=%d, got %d", e, a)
	}
	{
		bpr := cap.BytesPerReplica
		if bpr.P10 != bpr.P25 {
			t.Errorf("expected BytesPerReplica p10 and p25 to be equal with 2 replicas, got %+v", bpr)
		}
		if bpr.P50 != bpr.P75 || bpr.P50 != bpr.P90 {
			t.Errorf("expected BytesPerReplica p50, p75, and p90 to be equal with 2 replicas, got %+v", bpr)
		}
		if bpr.P10 == bpr.P90 {
			t.Errorf("expected BytesPerReplica p10 and p90 to be different with 2 replicas, got %+v", bpr)
		}
	}
}

// TestRangeLookupAfterMeta2Split verifies that RangeLookup scans succeed even
// when user ranges span the boundary of two split meta2 ranges. We test this
// with forward and reverse ScanRequests so that we test both forward and
// reverse RangeLookups. In the case of both the RangeLookup scan directions,
// the forward part of the scan will need to continue onto a second range to
// find the desired RangeDescriptor (remember that a reverse RangeLookup
// includes an initial forward scan).
func TestRangeLookupAfterMeta2Split(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)

	// The following assumes that keys.TestingUserDescID(0) returns 50.
	//
	// Create a split at /Table/48 and /Meta2/Table/51. This creates:
	//   meta ranges [/Min-/Meta2/Table/51) and [/Meta2/Table/51-/System)
	//   user ranges [/Table/19-/Table/48)  and [/Table/48-/Max)
	//
	// Note that the two boundaries are offset such that a lookup for key /Table/49
	// will first search for meta(/Table/49) which is on the left meta2 range. However,
	// the user range [/Table/48-/Max) is stored on the right meta2 range, so the lookup
	// will require a scan that continues into the next meta2 range.
	tableID := bootstrap.TestingUserDescID(1) // 51
	splitReq := adminSplitArgs(keys.SystemSQLCodec.TablePrefix(tableID - 3 /* 48 */))
	if _, pErr := kv.SendWrapped(ctx, s.DB().NonTransactionalSender(), splitReq); pErr != nil {
		t.Fatal(pErr)
	}

	metaKey := keys.RangeMetaKey(roachpb.RKey(keys.SystemSQLCodec.TablePrefix(tableID))).AsRawKey()
	splitReq = adminSplitArgs(metaKey)
	if _, pErr := kv.SendWrapped(ctx, s.DB().NonTransactionalSender(), splitReq); pErr != nil {
		t.Fatal(pErr)
	}

	testutils.RunTrueAndFalse(t, "reverse", func(t *testing.T, rev bool) {
		// Clear the RangeDescriptorCache so that no cached descriptors are
		// available from previous lookups.
		s.DistSenderI().(*kvcoord.DistSender).RangeDescriptorCache().Clear()

		// Scan from [/Table/49-/Table/50) both forwards and backwards.
		// Either way, the resulting RangeLookup scan will be forced to
		// perform a continuation lookup.
		scanStart := keys.SystemSQLCodec.TablePrefix(tableID - 2) // 49
		scanEnd := scanStart.PrefixEnd()                          // 50
		header := kvpb.RequestHeader{
			Key:    scanStart,
			EndKey: scanEnd,
		}

		var lookupReq kvpb.Request
		if rev {
			// A ReverseScanRequest will trigger a reverse RangeLookup scan.
			lookupReq = &kvpb.ReverseScanRequest{RequestHeader: header}
		} else {
			lookupReq = &kvpb.ScanRequest{RequestHeader: header}
		}
		if _, err := kv.SendWrapped(ctx, s.DB().NonTransactionalSender(), lookupReq); err != nil {
			t.Fatalf("%T %v", err.GoError(), err)
		}
	})
}

// TestStoreSplitRangeLookupRace verifies that a RangeLookup scanning across
// multiple meta2 ranges that races with a split and misses all matching
// descriptors will retry its scan until it succeeds.
//
// This test creates a series of events that result in the injected range
// lookup scan response we see in TestRangeLookupRaceSplits/MissingDescriptor.
// It demonstrates how it is possible for an inconsistent range lookup scan
// that spans multiple ranges to completely miss its desired descriptor.
func TestStoreSplitRangeLookupRace(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// The scenario is modeled after:
	// https://github.com/cockroachdb/cockroach/issues/19147#issuecomment-336741791
	// See that comment for a description of why a non-transactional scan
	// starting at "/meta2/k" may only see non-matching descriptors when racing
	// with a split.
	//
	// To simulate this situation, we first perform splits at "/meta2/n", "j",
	// and "p". This creates the following structure, where the descriptor for
	// range [j, p) is stored on the second meta2 range:
	//
	//   [/meta2/a,/meta2/n), [/meta2/n,/meta2/z)
	//                     -----^
	//       ...      [j, p)      ...
	//
	// We then initiate a range lookup for key "k". This lookup will begin
	// scanning on the first meta2 range but won't find its desired desriptor. Normally,
	// it would continue scanning onto the second meta2 range and find the descriptor
	// for range [j, p) at "/meta2/p" (see TestRangeLookupAfterMeta2Split). However,
	// because RangeLookup scans are non-transactional, this can race with a split.
	// Here, we split at key "m", which creates the structure:
	//
	//   [/meta2/a,/meta2/n), [/meta2/n,/meta2/z)
	//             ^--        ---^
	//       ...   [j,m), [m,p)      ...
	//
	// If the second half of the RangeLookup scan sees the second meta2 range after
	// this split, it will miss the old descriptor for [j, p) and the new descriptor
	// for [j, m). In this case, the RangeLookup should retry.
	lookupKey := roachpb.Key("k")
	bounds, err := keys.MetaScanBounds(keys.RangeMetaKey(roachpb.RKey(lookupKey)))
	if err != nil {
		t.Fatal(err)
	}

	// The following filter and set of channels is used to block the RangeLookup
	// scan for key "k" after it has scanned over the first meta2 range but not
	// the second.
	blockRangeLookups := make(chan struct{})
	blockedRangeLookups := int32(0)
	rangeLookupIsBlocked := make(chan struct{}, 1)
	unblockRangeLookups := make(chan struct{})
	respFilter := func(ctx context.Context, ba *kvpb.BatchRequest, _ *kvpb.BatchResponse) *kvpb.Error {
		select {
		case <-blockRangeLookups:
			if kv.TestingIsRangeLookup(ba) &&
				ba.Requests[0].GetInner().Header().Key.Equal(bounds.Key.AsRawKey()) {

				select {
				case rangeLookupIsBlocked <- struct{}{}:
					atomic.AddInt32(&blockedRangeLookups, 1)
				default:
				}
				<-unblockRangeLookups
			}
		default:
		}
		return nil
	}

	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableSplitQueue:     true,
				DisableMergeQueue:     true,
				TestingResponseFilter: respFilter,
				IntentResolverKnobs: kvserverbase.IntentResolverTestingKnobs{
					ForceSyncIntentResolution: true,
				},
			},
		},
	})

	defer s.Stopper().Stop(context.Background())
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	if err != nil {
		t.Fatal(err)
	}

	mustSplit := func(splitKey roachpb.Key) {
		args := adminSplitArgs(splitKey)

		// Don't use s.DistSenderI().(kv.Sender) so that we don't disturb the RangeDescriptorCache.
		rangeID := store.LookupReplica(roachpb.RKey(splitKey)).RangeID
		_, pErr := kv.SendWrappedWith(context.Background(), store, kvpb.Header{
			RangeID: rangeID,
		}, args)
		if pErr != nil {
			t.Fatal(pErr)
		}
	}

	// Perform the initial splits. See above.
	mustSplit(keys.SystemPrefix)
	mustSplit(keys.RangeMetaKey(roachpb.RKey("n")).AsRawKey())
	mustSplit(roachpb.Key("j"))
	mustSplit(roachpb.Key("p"))

	// Launch a goroutine to perform a range lookup for key "k" that will race
	// with a split at key "m".
	rangeLookupErr := make(chan error)
	go func() {
		close(blockRangeLookups)

		// Loop until at-least one range lookup is triggered and blocked.
		// This accommodates for races with in-flight range lookups.
		var err error
		for atomic.LoadInt32(&blockedRangeLookups) == 0 && err == nil {
			// Clear the RangeDescriptorCache to trigger a range lookup when the
			// lookupKey is next accessed. Then immediately access lookupKey.
			s.DistSenderI().(*kvcoord.DistSender).RangeDescriptorCache().Clear()
			_, err = s.DB().Get(context.Background(), lookupKey)
		}
		rangeLookupErr <- err
	}()

	// Wait until the range lookup is blocked after performing a scan of the
	// first range [/meta2/a,/meta2/n) but before performing a scan of the
	// second range [/meta2/n,/meta2/z). Then split at key "m". Finally, let the
	// range lookup finish. The lookup will fail because it won't get consistent
	// results but will eventually succeed after retrying.
	select {
	case <-rangeLookupIsBlocked:
	case err := <-rangeLookupErr:
		// Unexpected early return.
		t.Fatalf("unexpected range lookup error %v", err)
	}
	mustSplit(roachpb.Key("m"))
	close(unblockRangeLookups)

	if err := <-rangeLookupErr; err != nil {
		t.Fatalf("unexpected range lookup error %v", err)
	}
}

// Verify that range lookup operations do not synchronously perform intent
// resolution as doing so can deadlock with the RangeDescriptorCache. See
// #17760.
func TestRangeLookupAsyncResolveIntent(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	blockPushTxn := make(chan struct{})
	defer close(blockPushTxn)

	testingProposalFilter :=
		func(args kvserverbase.ProposalFilterArgs) *kvpb.Error {
			for _, union := range args.Req.Requests {
				if union.GetInner().Method() == kvpb.PushTxn {
					<-blockPushTxn
					break
				}
			}
			return nil
		}
	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				// Disable async tasks in the intent resolver. All tasks will be synchronous.
				IntentResolverKnobs: kvserverbase.IntentResolverTestingKnobs{
					ForceSyncIntentResolution: true,
				},
				DisableMergeQueue:     true,
				DisableSplitQueue:     true,
				TestingProposalFilter: testingProposalFilter,
			},
		},
	})

	defer s.Stopper().Stop(context.Background())
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Split range 1 at an arbitrary key so that we're not dealing with the
	// first range for the rest of this test. The first range is handled
	// specially by the range descriptor cache.
	key := roachpb.Key("a")
	args := adminSplitArgs(key)
	if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
		t.Fatal(pErr)
	}

	// Get original meta2 descriptor.
	rs, _, err := kv.RangeLookup(ctx, store.TestSender(), key, kvpb.READ_UNCOMMITTED, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	origDesc := rs[0]

	key2 := roachpb.Key("e")
	newDesc := origDesc
	newDesc.EndKey, err = keys.Addr(key2)
	if err != nil {
		t.Fatal(err)
	}

	// Write the new descriptor as an intent.
	data, err := protoutil.Marshal(&newDesc)
	if err != nil {
		t.Fatal(err)
	}
	txn := roachpb.MakeTransaction("test", key2, isolation.Serializable, 1, store.Clock().Now(), store.Clock().MaxOffset().Nanoseconds(), int32(s.SQLInstanceID()), 0, false /* omitInRangefeeds */)
	// Officially begin the transaction. If not for this, the intent resolution
	// machinery would simply remove the intent we write below, see #3020.
	// We send directly to Replica throughout this test, so there's no danger
	// of the Store aborting this transaction (i.e. we don't have to set a high
	// priority).
	pArgs := putArgs(keys.RangeMetaKey(roachpb.RKey(key2)).AsRawKey(), data)
	txn.Sequence++
	pArgs.Sequence = txn.Sequence
	if _, pErr := kv.SendWrappedWith(ctx, store.TestSender(), kvpb.Header{Txn: &txn}, pArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Clear the range descriptor cache so that any future requests will first
	// need to perform a RangeLookup.
	store.DB().NonTransactionalSender().(*kv.CrossRangeTxnWrapperSender).Wrapped().(*kvcoord.DistSender).RangeDescriptorCache().Clear()

	// Now send a request, forcing the RangeLookup. Since the lookup is
	// inconsistent, there's no LockConflictError, but we'll try to resolve any
	// intents that are found. If the RangeLookup op attempts to resolve the
	// intents synchronously, the operation will block forever.
	//
	// Note that 'a' < 'e'.
	if _, err := store.DB().Get(ctx, key); err != nil {
		t.Fatal(err)
	}
}

// Verify that replicas don't temporarily disappear from the replicas map during
// the splits. See #29144.
func TestStoreSplitDisappearingReplicas(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)
	go kvserver.WatchForDisappearingReplicas(t, store)
	for i := 0; i < 100; i++ {
		key := roachpb.Key(fmt.Sprintf("a%d", i))
		args := adminSplitArgs(key)
		if _, pErr := kv.SendWrapped(context.Background(), store.TestSender(), args); pErr != nil {
			t.Fatalf("%q: split unexpected error: %s", key, pErr)
		}
	}
}

// Regression test for #21146. This verifies the behavior of when the
// application of some split command (part of the lhs's log) is delayed on some
// store and meanwhile the rhs has rebalanced away and back, ending up with a
// larger ReplicaID than the split thinks it will have. Additionally we remove
// the LHS replica on the same store before the split and re-add it after, so
// that when the connectivity restores the LHS will apply a split trigger while
// it is not a part of the descriptor.
//
// Or, in pictures (s3 looks like s1 throughout and is omitted):
//
//	s1:  [----r1@all-------------]
//	s2:  [----r1@all-------------]
//
// Remove s2:
//
//	s1:  [----r1@s1s3------------]
//	s2:  [----r1@all-------------] (outdated)
//
// Split r1:
//
//	s1:  [-r1@s1s3-|--r2@s1s3----]
//	s2:  [----r1@all-------------] (outdated)
//
// Add s2:
//
//	s1:  [-r1@all-|--r2@s1s3-----]
//	s2:  [----r1@all-------------] (outdated)
//
// Add learner to s2 on r2 (remains uninitialized due to LHS state blocking it):
//
//	s1:  [-r1@s1s3-|--r2@all-----]
//	s2:  [----r1@all-------------] (outdated), uninitialized replica r2/3
//
// Remove and re-add learner multiple times: r2/3 becomes r2/100
//
//	(diagram looks the same except for replacing r2/3)
//
// When connectivity is restored, r1@s2 will start to catch up on the raft log
// after it learns of its new replicaID. It first processes the replication
// change that removes it and switches to a desc that doesn't contain itself as
// a replica. Next it sees the split trigger that once caused a crash because
// the store tried to look up itself and failed. This being handled correctly,
// the split trigger next has to look up the right hand side, which surprisingly
// has a higher replicaID than that seen in the split trigger. This too needs to
// be tolerated.
func TestSplitTriggerMeetsUnexpectedReplicaID(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	blockPromoteCh := make(chan struct{})
	var skipSnaps int32
	withoutLearnerSnap := func(fn func()) {
		atomic.StoreInt32(&skipSnaps, 1)
		fn()
		atomic.StoreInt32(&skipSnaps, 0)
	}
	knobs := base.TestingKnobs{Store: &kvserver.StoreTestingKnobs{
		ReplicaSkipInitialSnapshot: func() bool {
			return atomic.LoadInt32(&skipSnaps) != 0
		},
		RaftSnapshotQueueSkipReplica: func() bool {
			return atomic.LoadInt32(&skipSnaps) != 0
		},
		VoterAddStopAfterLearnerSnapshot: func(targets []roachpb.ReplicationTarget) bool {
			if atomic.LoadInt32(&skipSnaps) != 0 {
				return false
			}
			if len(targets) > 0 && targets[0].StoreID == 2 {
				<-blockPromoteCh
			}
			return false
		},
		ReplicaAddSkipLearnerRollback: func() bool {
			return true
		},
		// We rely on replicas remaining where they are even when they are removed
		// from the range as this lets us set up a split trigger that will apply
		// on a replica that is (at the time of the split trigger) not a member.
		DisableReplicaGCQueue: true,
	}}
	tc := testcluster.StartTestCluster(t, 3, base.TestClusterArgs{
		ServerArgs:      base.TestServerArgs{Knobs: knobs},
		ReplicationMode: base.ReplicationManual,
	})
	defer tc.Stopper().Stop(ctx)

	k := tc.ScratchRange(t)
	desc := tc.LookupRangeOrFatal(t, k)

	// Add a replica on n3 which we'll need to achieve quorum while we cut off n2 below.
	tc.AddVotersOrFatal(t, k, tc.Target(2))

	// First construct a range with a learner replica on the second node (index 1)
	// and split it, ending up with an orphaned learner on each side of the split.
	// After the learner is created, but before the split, block all incoming raft
	// traffic to the learner on the lhs of the split (which is still on the
	// second node).
	g := ctxgroup.WithContext(ctx)
	g.GoCtx(func(ctx context.Context) error {
		_, err := tc.Servers[0].DB().AdminChangeReplicas(
			ctx, k, tc.LookupRangeOrFatal(t, k), kvpb.MakeReplicationChanges(roachpb.ADD_VOTER, tc.Target(1)),
		)
		return err
	})

	store, _ := getFirstStoreReplica(t, tc.Server(1), k)
	tc.Servers[1].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(store.StoreID(), &unreliableRaftHandler{
		rangeID:                    desc.RangeID,
		IncomingRaftMessageHandler: store,
	})

	_, kRHS := k, k.Next()
	// Remove the LHS on the isolated store, split the range, and re-add it.
	tc.RemoveVotersOrFatal(t, k, tc.Target(1))
	descLHS, descRHS := tc.SplitRangeOrFatal(t, kRHS)
	withoutLearnerSnap(func() {
		// NB: can't use AddVoters since that waits for the target to be up
		// to date, which it won't in this case.
		//
		// We avoid sending a snapshot because that snapshot would include the
		// split trigger and we want that to be processed via the log.
		d, err := tc.Servers[0].DB().AdminChangeReplicas(
			ctx, descLHS.StartKey.AsRawKey(), descLHS, kvpb.MakeReplicationChanges(roachpb.ADD_VOTER, tc.Target(1)),
		)
		require.NoError(t, err)
		descLHS = *d
	})

	close(blockPromoteCh)
	if err := g.Wait(); !testutils.IsError(err, `descriptor changed`) {
		t.Fatalf(`expected "descriptor changed" error got: %+v`, err)
	}

	// Now repeatedly re-add the learner on the rhs, so it has a
	// different replicaID than the split trigger expects.
	add := func() {
		_, err := tc.Servers[0].DB().AdminChangeReplicas(
			ctx, kRHS, tc.LookupRangeOrFatal(t, kRHS), kvpb.MakeReplicationChanges(roachpb.ADD_VOTER, tc.Target(1)),
		)
		// The "snapshot intersects existing range" error is expected if the store
		// has not heard a raft message addressed to a later replica ID while the
		// "was not found on" error is expected if the store has heard that it has
		// a newer replica ID before receiving the snapshot.
		if !testutils.IsError(err, `snapshot intersects existing range|r[0-9]+ was not found on s[0-9]+`) {
			t.Fatalf(`expected snapshot intersects existing range|r[0-9]+ was not found on s[0-9]+" error got: %+v`, err)
		}
	}
	for i := 0; i < 5; i++ {
		add()
		tc.RemoveVotersOrFatal(t, kRHS, tc.Target(1))
	}
	add()

	// Normally AddVoters will return the latest version of the RangeDescriptor,
	// but because we're getting snapshot errors and using the
	// ReplicaAddSkipLearnerRollback hook, we have to look it up again ourselves
	// to find the current replicaID for the RHS learner.
	descRHS = tc.LookupRangeOrFatal(t, kRHS)
	learnerDescRHS, ok := descRHS.GetReplicaDescriptor(store.StoreID())
	require.True(t, ok)

	// Wait for there to be an in-memory, uninitialized learner replica with the
	// latest ReplicaID. Note: it cannot become initialized at this point because
	// it needs a snapshot to do that and (as can be seen in the error check
	// above) snapshots will intersect the lhs replica (which doesn't know about
	// the split because we've blocked its raft traffic, and so it still covers
	// the pre-split keyspace).
	testutils.SucceedsSoon(t, func() error {
		repl, err := store.GetReplica(descRHS.RangeID)
		if err != nil {
			return err
		}
		status := repl.RaftStatus()
		if status == nil {
			return errors.New("raft group not initialized")
		}
		if replicaID := roachpb.ReplicaID(status.ID); replicaID != learnerDescRHS.ReplicaID {
			return errors.Errorf("expected %d got %d", learnerDescRHS.ReplicaID, replicaID)
		}
		return nil
	})

	// Re-enable raft and wait for the lhs to catch up to the post-split
	// descriptor. This used to panic with "raft group deleted".
	tc.Servers[1].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(store.StoreID(), store)
	testutils.SucceedsSoon(t, func() error {
		repl, err := store.GetReplica(descLHS.RangeID)
		if err != nil {
			return err
		}
		if desc := repl.Desc(); desc.IsInitialized() && !descLHS.Equal(desc) {
			require.NoError(t, store.ManualReplicaGC(repl))
			return errors.Errorf("expected %s got %s", &descLHS, desc)
		}
		return nil
	})
}

// TestSplitBlocksReadsToRHS tests that an ongoing range split does not
// interrupt reads to the LHS of the split but does interrupt reads for the RHS
// of the split. The test relies on the fact that EndTxn(SplitTrigger) declares
// read access to the LHS of the split but declares write access to the RHS of
// the split.
func TestSplitBlocksReadsToRHS(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	keyLHS, keySplit, keyRHS := roachpb.Key("a"), roachpb.Key("b"), roachpb.Key("c")
	splitBlocked := make(chan struct{})
	propFilter := func(args kvserverbase.ProposalFilterArgs) *kvpb.Error {
		if req, ok := args.Req.GetArg(kvpb.EndTxn); ok {
			et := req.(*kvpb.EndTxnRequest)
			if tr := et.InternalCommitTrigger.GetSplitTrigger(); tr != nil {
				if tr.RightDesc.StartKey.Equal(keySplit) {
					// Signal that the split is blocked.
					splitBlocked <- struct{}{}
					// Wait for split to be unblocked.
					<-splitBlocked
				}
			}
		}
		return nil
	}

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:     true,
				DisableSplitQueue:     true,
				TestingProposalFilter: propFilter,
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	repl := store.LookupReplica(roachpb.RKey(keySplit))
	tsBefore := store.Clock().Now()

	// Begin splitting the range.
	g := ctxgroup.WithContext(ctx)
	g.GoCtx(func(ctx context.Context) error {
		args := adminSplitArgs(keySplit)
		_, pErr := kv.SendWrapped(ctx, store.TestSender(), args)
		return pErr.GoError()
	})

	// Wait until split is underway.
	<-splitBlocked
	tsAfter := store.Clock().Now()

	// Read from the LHS and RHS, both below and above the split timestamp.
	lhsDone, rhsDone := make(chan error, 2), make(chan error, 2)
	for _, keyAndChan := range []struct {
		key   roachpb.Key
		errCh chan error
	}{
		{keyLHS, lhsDone},
		{keyRHS, rhsDone},
	} {
		for _, ts := range []hlc.Timestamp{tsBefore, tsAfter} {
			h := kvpb.Header{Timestamp: ts, RangeID: repl.RangeID}
			args := getArgs(keyAndChan.key)
			errCh := keyAndChan.errCh
			g.GoCtx(func(ctx context.Context) error {
				// Send directly to repl to avoid racing with the
				// split and routing requests to the post-split RHS.
				_, pErr := kv.SendWrappedWith(ctx, repl, h, args)
				errCh <- pErr.GoError()
				return nil
			})
		}
	}

	// Only the LHS reads should succeed. The RHS reads should get
	// blocked waiting to acquire latches.
	for i := 0; i < cap(lhsDone); i++ {
		require.NoError(t, <-lhsDone)
	}
	select {
	case err := <-rhsDone:
		require.NoError(t, err)
		t.Fatal("unexpected read on RHS during split")
	case <-time.After(2 * time.Millisecond):
	}

	// Unblock the split.
	splitBlocked <- struct{}{}

	// The RHS reads should now both hit a RangeKeyMismatchError error.
	for i := 0; i < cap(rhsDone); i++ {
		require.Regexp(t, "outside of bounds of range", <-rhsDone)
	}
	require.Nil(t, g.Wait())
}

// TestStoreRangeSplitAndMergeWithGlobalReads tests that a range configured to
// serve global reads can be split and merged. In essence, this tests whether
// the split and merge transactions can handle having their timestamp bumped by
// the closed timestamp on the ranges they're operating on.
func TestStoreRangeSplitAndMergeWithGlobalReads(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Detect splits and merges over the global read ranges. Assert that the split
	// and merge transactions commit with pushed write timestamps, and that the
	// commit-wait sleep for these transactions is performed before running their
	// commit triggers instead of run on the kv client. For details on why this is
	// necessary, see maybeCommitWaitBeforeCommitTrigger.
	var clockPtr atomic.Pointer[hlc.Clock]
	var splits, merges int64
	respFilter := func(ctx context.Context, ba *kvpb.BatchRequest, br *kvpb.BatchResponse) *kvpb.Error {
		clock := clockPtr.Load()
		if clock == nil {
			return nil
		}
		if req, ok := ba.GetArg(kvpb.EndTxn); ok {
			endTxn := req.(*kvpb.EndTxnRequest)
			if br.Txn.Status == roachpb.COMMITTED && br.Txn.MinTimestamp.Less(br.Txn.WriteTimestamp) {
				if ct := endTxn.InternalCommitTrigger; ct != nil {
					// The server-side commit-wait sleep should ensure that the commit
					// triggers are only run after the commit timestamp is below present
					// time.
					now := clock.Now()
					require.True(t, br.Txn.WriteTimestamp.Less(now))

					switch {
					case ct.SplitTrigger != nil:
						atomic.AddInt64(&splits, 1)
					case ct.MergeTrigger != nil:
						atomic.AddInt64(&merges, 1)
					}
				}
			}
		}
		return nil
	}

	descID := bootstrap.TestingUserDescID(0)
	descKey := keys.SystemSQLCodec.TablePrefix(descID)
	splitKey := append(descKey, []byte("split")...)

	// Set global reads for the test ranges.
	spanConfig := zonepb.DefaultZoneConfig().AsSpanConfig()
	spanConfig.GlobalReads = true

	ctx := context.Background()
	s := serverutils.StartServerOnly(t, base.TestServerArgs{
		DisableSQLServer: true,
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue:     true,
				TestingResponseFilter: respFilter,
			},
			SpanConfig: &spanconfig.TestingKnobs{
				StoreKVSubscriberOverride: func(original spanconfig.KVSubscriber) spanconfig.KVSubscriber {
					wrapped := spanconfigtestutils.NewWrappedKVSubscriber(original)
					wrapped.AddOverride(descKey, spanConfig)
					wrapped.AddOverride(splitKey, spanConfig)
					return wrapped
				},
			},
		},
	})

	defer s.Stopper().Stop(ctx)
	// Set the closed_timestamp interval to be short to shorten the test duration
	// because we need to wait for a checkpoint on the system config.
	store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
	require.NoError(t, err)

	// Split off the range for the test.
	splitArgs := adminSplitArgs(descKey)
	_, pErr := kv.SendWrapped(ctx, store.TestSender(), splitArgs)
	require.Nil(t, pErr)

	// Set the clock to the store's clock, which also serves to engage the
	// response filter.
	clockPtr.Store(s.Clock())

	// Verify that the closed timestamp policy is set up.
	repl := store.LookupReplica(roachpb.RKey(descKey))
	testutils.SucceedsSoon(t, func() error {
		if actual := repl.ClosedTimestampPolicy(); actual != roachpb.LEAD_FOR_GLOBAL_READS {
			return errors.Newf("expected LEAD_FOR_GLOBAL_READS, got %s", actual)
		}
		return nil
	})

	// Write to the range, which has the effect of bumping the closed timestamp.
	pArgs := putArgs(descKey, []byte("foo"))
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), pArgs)
	require.Nil(t, pErr)

	// Split the range. Should succeed.
	splitArgs = adminSplitArgs(splitKey)
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), splitArgs)
	require.Nil(t, pErr)
	require.Equal(t, int64(1), store.Metrics().CommitWaitsBeforeCommitTrigger.Count())
	require.Equal(t, int64(1), atomic.LoadInt64(&splits))

	repl = store.LookupReplica(roachpb.RKey(splitKey))
	require.Equal(t, splitKey, repl.Desc().StartKey.AsRawKey())

	// Merge the range. Should succeed.
	mergeArgs := adminMergeArgs(descKey)
	_, pErr = kv.SendWrapped(ctx, store.TestSender(), mergeArgs)
	require.Nil(t, pErr)
	require.Equal(t, int64(2), store.Metrics().CommitWaitsBeforeCommitTrigger.Count())
	require.Equal(t, int64(1), atomic.LoadInt64(&merges))

	repl = store.LookupReplica(roachpb.RKey(splitKey))
	require.Equal(t, descKey, repl.Desc().StartKey.AsRawKey())
}

// TestLBSplitUnsafeKeys tests that load based splits do not split between table
// rows, even when the suggested load based split key is itself between a table row.
func TestLBSplitUnsafeKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()
	const indexID = 1

	// The test is expensive and prone to timing out under race or deadlock.
	skip.UnderRace(t)
	skip.UnderDeadlock(t)

	makeTestKey := func(tableID uint32, suffix []byte) roachpb.Key {
		tableKey := keys.MakeTableIDIndexID(nil, tableID, indexID)
		return append(tableKey, suffix...)
	}

	es := func(vals ...int64) []byte {
		k := []byte{}
		for _, v := range vals {
			k = encoding.EncodeVarintAscending(k, v)
		}
		return k
	}

	fk := func(k []byte, famID uint32) []byte {
		return keys.MakeFamilyKey(k, famID)
	}

	testCases := []struct {
		splitKey     roachpb.Key
		existingKeys []int
		expSplitKey  roachpb.Key
		expErrStr    string
	}{
		// We don't know the table ID here, we append the splitKey to the
		// table/index prefix. e.g. /1 will be /Table/table_id/index_id/1.
		{
			// /1 -> /2
			splitKey:     es(1),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/0 -> /2
			splitKey:     fk(es(1), 0),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/3 -> /2
			splitKey:     fk(es(1), 3),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/0/0 -> /2
			splitKey:     roachpb.Key(fk(es(1), 0)).Next(),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/3/0 -> /2
			splitKey:     roachpb.Key(fk(es(1), 3)).Next(),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/0/0/0 -> /2
			splitKey:     roachpb.Key(fk(es(1), 0)).Next().Next(),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /1/3/0/0 -> /2
			splitKey:     roachpb.Key(fk(es(1), 3)).Next().Next(),
			existingKeys: []int{1, 2, 3},
			expSplitKey:  es(2),
		},
		{
			// /0 -> /3
			// We will not split at the first row in a table, so expect the split at
			// /3 intead of /2.
			splitKey:     es(0),
			existingKeys: []int{2, 3},
			expSplitKey:  es(3),
		},
		{
			// /2 -> /3
			// Same case as above, despite the key being safe, the split would create
			// an empty LHS.
			splitKey:     es(2),
			existingKeys: []int{2, 3},
			expSplitKey:  es(3),
		},
		{
			// /1 -> error
			// There are no rows to split on.
			splitKey:     es(1),
			existingKeys: []int{},
			expErrStr:    "could not find valid split key",
		},
		{
			// /1 -> error
			// There is only one row to split on, the range should not be split.
			splitKey:     es(1),
			existingKeys: []int{2},
			expErrStr:    "could not find valid split key",
		},
		{
			// /1/0 -> error
			splitKey:     fk(es(1), 0),
			existingKeys: []int{2},
			expErrStr:    "could not find valid split key",
		},
		{
			// /1/3 -> error
			splitKey:     fk(es(1), 3),
			existingKeys: []int{2},
			expErrStr:    "could not find valid split key",
		},
		{
			// /1/3/0/0 -> error
			splitKey:     roachpb.Key(fk(es(1), 0)).Next().Next(),
			existingKeys: []int{2},
			expErrStr:    "could not find valid split key",
		},
		{
			// /2 -> /2
			splitKey:     es(2),
			existingKeys: []int{1, 2},
			expSplitKey:  es(2),
		},
	}

	for _, tc := range testCases {
		var expectStr string
		if tc.expErrStr != "" {
			expectStr = tc.expErrStr
		} else {
			expectStr = makeTestKey(1, tc.expSplitKey).String()
		}
		t.Run(fmt.Sprintf("%s%v -> %s", makeTestKey(1, tc.splitKey), tc.existingKeys, expectStr), func(t *testing.T) {
			var targetRange atomic.Int32
			var splitKeyOverride atomic.Value
			splitKeyOverride.Store(roachpb.Key{})

			// Mock the load based splitter key finding method. This function will be
			// checked in splitQueue.shouldQueue() and splitQueue.process via
			// replica.loadSplitKey. When a key is returned, the split queue calls
			// replica.adminSplitWithDescriptor(...findFirstSafeSplitKey=true).
			overrideLBSplitFn := func(rangeID roachpb.RangeID) (splitKey roachpb.Key, useSplitKey bool) {
				if rangeID == roachpb.RangeID(targetRange.Load()) {
					override := splitKeyOverride.Load()
					// It is possible that the split queue is checking the range before
					// we manually enqueued it.
					if override == nil {
						return nil, false
					}
					overrideKey, ok := override.(roachpb.Key)
					require.Truef(t, ok, "stored value not key %+v", override)

					if len(overrideKey) == 0 {
						return nil, false
					}

					return override.(roachpb.Key), true
				}
				return nil, false
			}

			s, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{
				Knobs: base.TestingKnobs{
					Store: &kvserver.StoreTestingKnobs{
						LoadBasedSplittingOverrideKey: overrideLBSplitFn,
						DisableMergeQueue:             true,
					},
				},
			})

			defer s.Stopper().Stop(ctx)
			tdb := sqlutils.MakeSQLRunner(sqlDB)
			store, err := s.GetStores().(*kvserver.Stores).GetStore(1)
			require.NoError(t, err)

			// We want to exercise the case where there are column family keys.
			// Create a simple table and insert the existing keys.
			_ = tdb.Exec(t,
				"CREATE TABLE t (k INT PRIMARY KEY, "+
					"t0 INT, t1 INT, t2 INT, t3 INT, "+
					"FAMILY (k), FAMILY (t0), FAMILY (t1), FAMILY (t2), FAMILY (t3))")
			for _, k := range tc.existingKeys {
				_ = tdb.Exec(t, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, %d, %d, %d)",
					k, k, k, k, k))
			}

			// Force a table scan to resolve descriptors.
			var keyCount int
			tdb.QueryRow(t, "SELECT count(k) FROM t").Scan(&keyCount)
			require.Equal(t, len(tc.existingKeys), keyCount)
			var tableID uint32
			tdb.QueryRow(t, "SELECT table_id FROM crdb_internal.leases where name = 't'").Scan(&tableID)

			// Split off the table range for the test, otherwise the range may
			// contain multiple tables with existing values.
			splitArgs := adminSplitArgs(keys.SystemSQLCodec.TablePrefix(tableID))
			_, pErr := kv.SendWrapped(ctx, store.TestSender(), splitArgs)
			require.Nil(t, pErr)

			var rangeID roachpb.RangeID
			tdb.QueryRow(t, "SELECT range_id FROM [SHOW RANGES FROM TABLE t] LIMIT 1").Scan(&rangeID)
			targetRange.Store(int32(rangeID))
			repl, err := store.GetReplica(rangeID)
			require.NoError(t, err)

			// Keep the previous end key around, we will use this to assert that no
			// split has occurred when expecting an error.
			prevEndKey := repl.Desc().EndKey.AsRawKey()
			splitKey := makeTestKey(tableID, tc.splitKey)

			// Update the split key override so that the split queue will enqueue and
			// process the range. Remove it afterwards to avoid retrying the LHS.
			splitKeyOverride.Store(splitKey)
			processErr, enqueueErr := store.Enqueue(ctx, "split", repl, false /* shouldSkipQueue */, false /* async */)
			splitKeyOverride.Store(roachpb.Key{})
			require.NoError(t, enqueueErr)

			endKey := repl.Desc().EndKey.AsRawKey()
			if tc.expErrStr != "" {
				// We expect this split not to process, assert that the expected error
				// matches the returned error and the range has the same end key.
				require.ErrorContainsf(t, processErr, tc.expErrStr,
					"end key %s, previous end key %s", endKey, prevEndKey)
				require.Equal(t, prevEndKey, endKey)
			} else {
				// Otherwise, assert that the new range end key matches the expected
				// end key.
				require.NoError(t, processErr)
				require.Equal(t, makeTestKey(tableID, tc.expSplitKey), endKey)
			}
		})
	}
}

// TestSplitWithExternalFilesFastStats tests that while a range has
// external file bytes, we calculate an estimate of the stats during
// splits.
func TestSplitWithExternalFilesFastStats(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	defer nodelocal.ReplaceNodeLocalForTesting(t.TempDir())()
	const externURI = "nodelocal://1/external-files"
	ctx := context.Background()

	for _, fastStats := range []bool{true, false} {
		testName := fmt.Sprintf("%s=%v", kvserver.EnableEstimatedStatsForExternalBytes.InternalKey(), fastStats)
		t.Run(testName, func(t *testing.T) {
			st := cluster.MakeTestingClusterSettings()
			kvserver.EnableEstimatedStatsForExternalBytes.Override(ctx, &st.SV, fastStats)
			s := serverutils.StartServerOnly(t, base.TestServerArgs{
				Settings: st,
				Knobs: base.TestingKnobs{
					Store: &kvserver.StoreTestingKnobs{
						DisableMergeQueue:              true,
						DisableSplitQueue:              true,
						DisableCanAckBeforeApplication: true,
					},
				},
			})

			extStore, err := cloud.EarlyBootExternalStorageFromURI(ctx,
				externURI,
				base.ExternalIODirConfig{},
				s.ClusterSettings(),
				nil, /* limiters */
				cloud.NilMetrics)
			require.NoError(t, err)

			defer s.Stopper().Stop(ctx)
			store, err := s.GetStores().(*kvserver.Stores).GetStore(s.GetFirstStoreID())
			require.NoError(t, err)

			splitKey := roachpb.Key("b")
			// Link an external SST to the range with points on both
			// sides of our proposed split key.
			expKVs := []struct {
				Key   string
				Value string
			}{
				{Key: "a", Value: "a-val"},
				{Key: "c", Value: "c-val"},
			}
			kvs := make([]interface{}, 0, len(expKVs))
			for _, expKV := range expKVs {
				kvs = append(kvs, storageutils.PointKV(expKV.Key, 1, expKV.Value))
			}
			fileName := "external-1.sst"
			sst, _, _ := storageutils.MakeSST(t, s.ClusterSettings(), kvs)
			w, err := extStore.Writer(ctx, fileName)
			require.NoError(t, err)
			_, err = w.Write(sst)
			require.NoError(t, err)
			require.NoError(t, w.Close())

			size, err := extStore.Size(ctx, fileName)
			require.NoError(t, err)

			err = s.DB().LinkExternalSSTable(ctx, roachpb.Span{
				Key:    roachpb.Key("a"),
				EndKey: roachpb.Key("d"),
			}, kvpb.LinkExternalSSTableRequest_ExternalFile{
				Locator:                 externURI,
				Path:                    fileName,
				ApproximatePhysicalSize: uint64(size),
				BackingFileSize:         uint64(size),
				MVCCStats: &enginepb.MVCCStats{
					ContainsEstimates: 1,
					KeyBytes:          2,
					ValBytes:          10,
					KeyCount:          2,
					LiveCount:         2,
				},
			}, s.DB().Clock().Now())
			require.NoError(t, err)

			originalRepl := store.LookupReplica(roachpb.RKey(splitKey))
			require.NotNil(t, originalRepl)
			origStats, err := stateloader.Make(originalRepl.RangeID).LoadMVCCStats(ctx, store.TODOEngine())
			require.NoError(t, err)
			require.Greater(t, origStats.ContainsEstimates, int64(0), "range expected to have estimated stats")

			// Split the range.
			args := adminSplitArgs(splitKey)
			if _, pErr := kv.SendWrapped(ctx, store.TestSender(), args); pErr != nil {
				t.Fatal(pErr)
			}

			snap := store.StateEngine().NewSnapshot()
			defer snap.Close()
			lhsRepl := store.LookupReplica(originalRepl.Desc().StartKey)
			rhsRepl := store.LookupReplica(roachpb.RKey(splitKey))

			if !bytes.Equal(rhsRepl.Desc().StartKey, splitKey) || !bytes.Equal(splitKey, lhsRepl.Desc().EndKey) {
				t.Errorf("ranges mismatched, wanted %q=%q=%q", lhsRepl.Desc().EndKey, splitKey, rhsRepl.Desc().StartKey)
			}

			lhsStats, err := stateloader.Make(lhsRepl.RangeID).LoadMVCCStats(ctx, snap)
			require.NoError(t, err)
			rhsStats, err := stateloader.Make(rhsRepl.RangeID).LoadMVCCStats(ctx, snap)
			require.NoError(t, err)

			if fastStats {
				require.Greater(t, lhsStats.ContainsEstimates, int64(0), "lhs expected to have estimated stats")
				require.Greater(t, rhsStats.ContainsEstimates, int64(0), "rhs expected to have estimated stats")
			} else {
				estimates := kvserver.EnableEstimatedMVCCStatsInSplit.Get(&store.ClusterSettings().SV)
				recompute := kvserver.EnableMVCCStatsRecomputationInSplit.Get(&store.ClusterSettings().SV)
				if !estimates || (estimates && recompute) {
					now := s.Clock().Now()
					assertRecomputedStats(t, "lhs after split", snap, lhsRepl.Desc(), lhsStats, now.WallTime)
					assertRecomputedStats(t, "rhs after split", snap, rhsRepl.Desc(), rhsStats, now.WallTime)
				}
			}

			// Read back our values, just to be sure.
			for _, v := range expKVs {
				key := roachpb.Key(v.Key)
				gArgs := getArgs(key)
				reply, pErr := kv.SendWrapped(ctx, store.TestSender(), gArgs)
				require.NoError(t, pErr.GoError())

				replyBytes, err := reply.(*kvpb.GetResponse).Value.GetBytes()
				require.NoError(t, err)

				require.NoError(t, err)
				require.Equal(t, replyBytes, []byte(v.Value))
			}
		})
	}
}

// TestStoreRangeSplitRaftSnapshotAfterRHSRebalanced tests that a replica that
// learns about a split through a snapshot after the RHS has been rebalanced
// away correctly deletes the RHS replica's on-disk data.
//
// Serves as a regression test for
// https://github.com/cockroachdb/cockroach/issues/73462.
func TestStoreRangeSplitRaftSnapshotAfterRHSRebalanced(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.WithIssue(t, 73462)

	ctx := context.Background()
	// Start a 5 node cluster.
	tc := testcluster.StartTestCluster(t, 5, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
	})
	defer tc.Stopper().Stop(ctx)

	store0 := tc.GetFirstStoreFromServer(t, 0)
	store2 := tc.GetFirstStoreFromServer(t, 2)
	distSender := tc.Servers[0].DistSenderI().(kv.Sender)

	// Create a scratch range and upreplicate to stores 2 and 3.
	keyStart := tc.ScratchRange(t)
	repl := store0.LookupReplica(roachpb.RKey(keyStart))
	keyEnd := repl.Desc().EndKey.AsRawKey()
	keyD := keyStart.Next().Next().Next().Next()

	tc.AddVotersOrFatal(t, keyStart, tc.Targets(1, 2)...)
	tc.WaitForValues(t, keyStart, []int64{0, 0, 0, 0, 0})

	// Put some keys in [d, /Max) to ensure that the post-split RHS will have
	// some data. When we learn about the split through the snapshot, we expect
	// to delete data in the uncontained key range of [keyD, /Max). Adding some
	// data here allows us to make this assertion.
	key := keyD
	for i := 0; i < 10; i++ {
		key = key.Next()
		if _, pErr := kv.SendWrapped(ctx, distSender, incrementArgs(key, 1)); pErr != nil {
			t.Fatal(pErr)
		}
		tc.WaitForValues(t, key, []int64{1, 1, 1, 0, 0})
	}

	// Start dropping all Raft traffic to store2.
	aRepl0 := store0.LookupReplica(roachpb.RKey(keyStart))
	tc.Servers[2].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(store2.Ident.StoreID, &unreliableRaftHandler{
		rangeID:                    aRepl0.RangeID,
		IncomingRaftMessageHandler: store2,
		unreliableRaftHandlerFuncs: unreliableRaftHandlerFuncs{},
	})

	// Split at keyD: [keyStart, keyD) and [keyD, /Max)
	if _, pErr := kv.SendWrapped(ctx, distSender, adminSplitArgs(keyD)); pErr != nil {
		t.Fatal(pErr)
	}

	// Move the [keyD, /Max) replica from store2 to store4.
	rhsDesc := store0.LookupReplica(roachpb.RKey(keyD)).Desc()
	tc.RemoveVotersOrFatal(t, keyD, tc.Target(2))
	tc.AddVotersOrFatal(t, keyD, tc.Target(4))

	// Transfer the lease for [keyD, /Max) to store4.
	tc.TransferRangeLeaseOrFatal(t, *rhsDesc, tc.Target(4))

	// Move the [keyD, /Max) replica from store0 to store2. This allows us to
	// assert store0 and store2 have the same data below, once we've allowed
	// store2 to catch up via a snapshot.
	tc.RemoveVotersOrFatal(t, keyD, tc.Target(0))
	tc.AddVotersOrFatal(t, keyD, tc.Target(3))

	// Truncate the logs of the LHS to ensure store2 needs to be caught up using a
	// snapshot.
	index := func() kvpb.RaftIndex {
		repl := store0.LookupReplica(roachpb.RKey(keyStart))
		index := repl.GetLastIndex()
		truncArgs := &kvpb.TruncateLogRequest{
			RequestHeader: kvpb.RequestHeader{Key: keyStart},
			Index:         index + 1,
			RangeID:       repl.RangeID,
		}
		if _, err := kv.SendWrapped(ctx, distSender, truncArgs); err != nil {
			t.Fatal(err)
		}
		waitForTruncationForTesting(t, repl, index)
		return index
	}()

	beforeRaftSnaps := store2.Metrics().RangeSnapshotsAppliedByVoters.Count()

	tc.Servers[2].RaftTransport().(*kvserver.RaftTransport).ListenIncomingRaftMessages(store2.Ident.StoreID, &unreliableRaftHandler{
		rangeID:                    aRepl0.RangeID,
		IncomingRaftMessageHandler: store2,
		unreliableRaftHandlerFuncs: unreliableRaftHandlerFuncs{
			dropReq: func(req *kvserverpb.RaftMessageRequest) bool {
				// Make sure that even going forward no MsgApp for what we just
				// truncated can make it through. The Raft transport is asynchronous so
				// this is necessary to make the test pass reliably - otherwise the
				// follower on store2 may catch up without needing a snapshot, tripping
				// up the test.
				//
				// NB: the Index on the message is the log index that _precedes_ any of
				// the entries in the MsgApp, so filter where msg.Index < index, not <=
				// index.
				return req.Message.Type == raftpb.MsgApp && kvpb.RaftIndex(req.Message.Index) < index
			},
			dropHB:   func(*kvserverpb.RaftHeartbeat) bool { return false },
			dropResp: func(*kvserverpb.RaftMessageResponse) bool { return false },
		},
	})

	// Wait for all replicas to catch up. This will require a Raft snapshot to
	// store2.
	testutils.SucceedsSoon(t, func() error {
		afterRaftSnaps := store2.Metrics().RangeSnapshotsAppliedByVoters.Count()
		if afterRaftSnaps <= beforeRaftSnaps {
			return errors.New("expected store2 to apply at least 1 additional raft snapshot")
		}
		getKeySet := func(engine storage.Engine) map[string]struct{} {
			kvs, err := storage.Scan(context.Background(), engine, keyStart, keyEnd, 0)
			require.NoError(t, err)
			out := map[string]struct{}{}
			for _, kv := range kvs {
				out[string(kv.Key.Key)] = struct{}{}
			}
			return out
		}
		storeKeys0 := getKeySet(store0.StateEngine())
		storeKeys2 := getKeySet(store2.StateEngine())
		if !maps.Equal(storeKeys0, storeKeys2) {
			return fmt.Errorf(
				"store0 and store2 have different keys: %s != %s", storeKeys0, storeKeys2,
			)
		}
		return nil
	})
}
