// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

// Note that there's also a lease_test.go, in package sql_test.

package sql

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
)

func TestLeaseSet(t *testing.T) {
	defer leaktest.AfterTest(t)()

	type data struct {
		version    sqlbase.DescriptorVersion
		expiration int64
	}
	type insert data
	type remove data

	type newest struct {
		version sqlbase.DescriptorVersion
	}

	testData := []struct {
		op       interface{}
		expected string
	}{
		{newest{0}, "<nil>"},
		{insert{2, 3}, "2:3"},
		{newest{0}, "2:3"},
		{newest{2}, "2:3"},
		{newest{3}, "<nil>"},
		{remove{2, 3}, ""},
		{insert{2, 4}, "2:4"},
		{newest{0}, "2:4"},
		{newest{2}, "2:4"},
		{newest{3}, "<nil>"},
		{insert{3, 1}, "2:4 3:1"},
		{newest{0}, "3:1"},
		{newest{1}, "<nil>"},
		{newest{2}, "2:4"},
		{newest{3}, "3:1"},
		{newest{4}, "<nil>"},
		{insert{1, 1}, "1:1 2:4 3:1"},
		{newest{0}, "3:1"},
		{newest{1}, "1:1"},
		{newest{2}, "2:4"},
		{newest{3}, "3:1"},
		{newest{4}, "<nil>"},
		{remove{3, 1}, "1:1 2:4"},
		{remove{1, 1}, "2:4"},
		{remove{2, 4}, ""},
	}

	set := &leaseSet{}
	for i, d := range testData {
		switch op := d.op.(type) {
		case insert:
			s := &LeaseState{}
			s.Version = op.version
			s.expiration.Time = time.Unix(0, op.expiration)
			set.insert(s)
		case remove:
			s := &LeaseState{}
			s.Version = op.version
			s.expiration.Time = time.Unix(0, op.expiration)
			set.remove(s)
		case newest:
			n := set.findNewest(op.version)
			s := "<nil>"
			if n != nil {
				s = fmt.Sprintf("%d:%d", n.Version, n.Expiration().UnixNano())
			}
			if d.expected != s {
				t.Fatalf("%d: expected %s, but found %s", i, d.expected, s)
			}
			continue
		}
		if s := set.String(); d.expected != s {
			t.Fatalf("%d: expected %s, but found %s", i, d.expected, s)
		}
	}
}

func getNumLeases(ts *tableState) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.active.data)
}

func TestPurgeOldLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// We're going to block gossip so it doesn't come randomly and clear up the
	// leases we're artificially setting up.
	gossipSem := make(chan struct{}, 1)
	serverParams := base.TestServerArgs{
		Knobs: base.TestingKnobs{
			SQLLeaseManager: &LeaseManagerTestingKnobs{
				GossipUpdateEvent: func(cfg config.SystemConfig) {
					gossipSem <- struct{}{}
					<-gossipSem
				},
			},
		},
	}
	s, db, kvDB := serverutils.StartServer(t, serverParams)
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)
	// Block gossip.
	gossipSem <- struct{}{}
	defer func() {
		// Unblock gossip.
		<-gossipSem
	}()

	if _, err := db.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k CHAR PRIMARY KEY, v CHAR);
`); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")

	var tables []sqlbase.TableDescriptor
	var expiration hlc.Timestamp
	getLeases := func() {
		err := kvDB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			for i := 0; i < 3; i++ {
				table, exp, err := leaseManager.acquireFreshestFromStore(ctx, txn, tableDesc.ID)
				if err != nil {
					t.Fatal(err)
				}
				tables = append(tables, table)
				expiration = exp
				if err := leaseManager.Release(table); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	getLeases()
	ts := leaseManager.findTableState(tableDesc.ID, false)
	if numLeases := getNumLeases(ts); numLeases != 1 {
		t.Fatalf("found %d leases instead of 1", numLeases)
	}
	// Publish a new version for the table
	if _, err := leaseManager.Publish(context.TODO(), tableDesc.ID, func(*sqlbase.TableDescriptor) error {
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}

	getLeases()
	ts = leaseManager.findTableState(tableDesc.ID, false)
	if numLeases := getNumLeases(ts); numLeases != 2 {
		t.Fatalf("found %d leases instead of 1", numLeases)
	}
	if err := ts.purgeOldLeases(
		context.TODO(), kvDB, false, 2 /* minVersion */, leaseManager); err != nil {
		t.Fatal(err)
	}

	if numLeases := getNumLeases(ts); numLeases != 1 {
		t.Fatalf("found %d leases instead of 1", numLeases)
	}
	ts.mu.Lock()
	correctLease := ts.active.data[0].TableDescriptor.ID == tables[5].ID &&
		ts.active.data[0].TableDescriptor.Version == tables[5].Version
	correctExpiration := ts.active.data[0].expirationToHLC() == expiration
	ts.mu.Unlock()
	if !correctLease {
		t.Fatalf("wrong lease survived purge")
	}
	if !correctExpiration {
		t.Fatalf("wrong lease expiration survived purge")
	}
}

// Test that changing a descriptor's name updates the name cache.
func TestNameCacheIsUpdated(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)

	if _, err := db.Exec(`
CREATE DATABASE t;
CREATE DATABASE t1;
CREATE TABLE t.test (k CHAR PRIMARY KEY, v CHAR);
`); err != nil {
		t.Fatal(err)
	}

	// Populate the name cache.
	if _, err := db.Exec("SELECT * FROM t.test;"); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")

	// Rename.
	if _, err := db.Exec("ALTER TABLE t.test RENAME TO t.test2;"); err != nil {
		t.Fatal(err)
	}

	// Check that the cache has been updated.
	if leaseManager.tableNames.get(tableDesc.ParentID, "test", s.Clock()) != nil {
		t.Fatalf("old name still in cache")
	}

	lease := leaseManager.tableNames.get(tableDesc.ParentID, "test2", s.Clock())
	if lease == nil {
		t.Fatalf("new name not found in cache")
	}
	if lease.ID != tableDesc.ID {
		t.Fatalf("new name has wrong ID: %d (expected: %d)", lease.ID, tableDesc.ID)
	}
	if err := leaseManager.Release(lease.TableDescriptor); err != nil {
		t.Fatal(err)
	}

	// Rename to a different database.
	if _, err := db.Exec("ALTER TABLE t.test2 RENAME TO t1.test2;"); err != nil {
		t.Fatal(err)
	}

	// Re-read the descriptor, to get the new ParentID.
	newTableDesc := sqlbase.GetTableDescriptor(kvDB, "t1", "test2")
	if tableDesc.ParentID == newTableDesc.ParentID {
		t.Fatalf("database didn't change")
	}

	// Check that the cache has been updated.
	if leaseManager.tableNames.get(tableDesc.ParentID, "test2", s.Clock()) != nil {
		t.Fatalf("old name still in cache")
	}

	lease = leaseManager.tableNames.get(newTableDesc.ParentID, "test2", s.Clock())
	if lease == nil {
		t.Fatalf("new name not found in cache")
	}
	if lease.ID != tableDesc.ID {
		t.Fatalf("new name has wrong ID: %d (expected: %d)", lease.ID, tableDesc.ID)
	}
	if err := leaseManager.Release(lease.TableDescriptor); err != nil {
		t.Fatal(err)
	}
}

// Tests that a name cache entry with by an expired lease is not returned.
func TestNameCacheEntryDoesntReturnExpiredLease(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)

	const tableName = "test"

	if _, err := db.Exec(fmt.Sprintf(`
CREATE DATABASE t;
CREATE TABLE t.%s (k CHAR PRIMARY KEY, v CHAR);
`, tableName)); err != nil {
		t.Fatal(err)
	}

	// Populate the name cache.
	if _, err := db.Exec("SELECT * FROM t.test;"); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", tableName)

	// Check the assumptions this tests makes: that there is a cache entry
	// (with a valid lease).
	if lease := leaseManager.tableNames.get(tableDesc.ParentID, tableName, s.Clock()); lease == nil {
		t.Fatalf("name cache has no unexpired entry for (%d, %s)", tableDesc.ParentID, tableName)
	} else {
		if err := leaseManager.Release(lease.TableDescriptor); err != nil {
			t.Fatal(err)
		}
	}

	leaseManager.ExpireLeases(s.Clock())

	// Check the name no longer resolves.
	if lease := leaseManager.tableNames.get(tableDesc.ParentID, tableName, s.Clock()); lease != nil {
		t.Fatalf("name cache has unexpired entry for (%d, %s): %s", tableDesc.ParentID, tableName, lease)
	}
}

// Test that table names are not treated as case sensitive by the name cache.
func TestTableNameNotCaseSensitive(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)

	if _, err := db.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k CHAR PRIMARY KEY, v CHAR);
`); err != nil {
		t.Fatal(err)
	}

	// Populate the name cache.
	if _, err := db.Exec("SELECT * FROM t.test;"); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")

	// Check that we can get the table by a different name.
	lease := leaseManager.tableNames.get(tableDesc.ParentID, "tEsT", s.Clock())
	if lease == nil {
		t.Fatalf("no name cache entry")
	}
	if err := leaseManager.Release(lease.TableDescriptor); err != nil {
		t.Fatal(err)
	}
}

// Test that there's no deadlock between AcquireByName and Release.
// We used to have one due to lock inversion between the tableNameCache lock and
// the leaseState lock, triggered when the same lease was Release()d after the
// table had been dropped (which means it's removed from the tableNameCache) and
// AcquireByName()d at the same time.
func TestReleaseAcquireByNameDeadlock(t *testing.T) {
	defer leaktest.AfterTest(t)()
	removalTracker := NewLeaseRemovalTracker()
	testingKnobs := base.TestingKnobs{
		SQLLeaseManager: &LeaseManagerTestingKnobs{
			LeaseStoreTestingKnobs: LeaseStoreTestingKnobs{
				LeaseReleasedEvent: removalTracker.LeaseRemovedNotification,
			},
		},
	}
	s, sqlDB, kvDB := serverutils.StartServer(
		t, base.TestServerArgs{Knobs: testingKnobs})
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)

	if _, err := sqlDB.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k CHAR PRIMARY KEY, v CHAR);
`); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")

	// Populate the name cache.
	var table sqlbase.TableDescriptor
	if err := kvDB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
		var err error
		table, _, err = leaseManager.AcquireByName(ctx, txn, tableDesc.ParentID, "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := leaseManager.Release(table); err != nil {
		t.Fatal(err)
	}

	// Pretend the table has been dropped, so that when we release leases on it,
	// they are removed from the tableNameCache too.
	tableState := leaseManager.findTableState(tableDesc.ID, true)
	tableState.dropped = true

	// Try to trigger the race repeatedly: race an AcquireByName against a
	// Release.
	// tableChan acts as a barrier, synchornizing the two routines at every
	// iteration.
	tableChan := make(chan sqlbase.TableDescriptor)
	errChan := make(chan error)
	go func() {
		for table := range tableChan {
			// Move errors to the main goroutine.
			errChan <- leaseManager.Release(table)
		}
	}()

	for i := 0; i < 50; i++ {
		var tableByName sqlbase.TableDescriptor
		if err := kvDB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
			var err error
			table, _, err := leaseManager.AcquireByName(ctx, txn, tableDesc.ParentID, "test")
			if err != nil {
				t.Fatal(err)
			}
			// This test will need to wait until leases are removed from the store
			// before creating new leases because the jitter used in the leases'
			// expiration causes duplicate key errors when trying to create new
			// leases. This is not a problem in production, since leases are not
			// removed from the store until they expire, and the jitter is small
			// compared to their lifetime, but it is a problem in this test because
			// we churn through leases quickly.
			tracker := removalTracker.TrackRemoval(table)
			// Start the race: signal the other guy to release, and we do another
			// acquire at the same time.
			tableChan <- table
			tableByName, _, err = leaseManager.AcquireByName(ctx, txn, tableDesc.ParentID, "test")
			if err != nil {
				t.Fatal(err)
			}
			tracker2 := removalTracker.TrackRemoval(tableByName)
			// See if there was an error releasing lease.
			err = <-errChan
			if err != nil {
				t.Fatal(err)
			}

			// Depending on how the race went, there are two cases - either the
			// AcquireByName ran first, and got the same lease as we already had,
			// or the Release ran first and so we got a new lease.
			if tableByName.ID == table.ID {
				if err := leaseManager.Release(table); err != nil {
					t.Fatal(err)
				}
				if err := tracker.WaitForRemoval(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := leaseManager.Release(tableByName); err != nil {
					t.Fatal(err)
				}
				if err := tracker2.WaitForRemoval(); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(tableChan)
}

// TestAcquireFreshestFromStoreRaces runs
// LeaseManager.acquireFreshestFromStore() in parallel to test for races.
func TestAcquireFreshestFromStoreRaces(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())
	leaseManager := s.LeaseManager().(*LeaseManager)

	if _, err := db.Exec(`
CREATE DATABASE t;
CREATE TABLE t.test (k CHAR PRIMARY KEY, v CHAR);
`); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")

	var wg sync.WaitGroup
	numRoutines := 10
	wg.Add(numRoutines)
	for i := 0; i < numRoutines; i++ {
		go func() {
			defer wg.Done()
			err := kvDB.Txn(context.TODO(), func(ctx context.Context, txn *client.Txn) error {
				table, _, err := leaseManager.acquireFreshestFromStore(ctx, txn, tableDesc.ID)
				if err != nil {
					return err
				}
				if err := leaseManager.Release(table); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
