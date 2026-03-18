// pd-cluster-verify verifies cross-node data replication in a running gookvs cluster with PD.
// It discovers the leader via PD, writes data to it, and reads from follower nodes.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var pdAddr = "127.0.0.1:2379"

func main() {
	fmt.Println("=== gookvs PD Cluster Verification ===")
	fmt.Println()

	// Step 1: Connect to PD and verify it's healthy.
	fmt.Println("[1/4] PD server check...")
	pdCtx, pdCancel := context.WithTimeout(context.Background(), 5*time.Second)
	pdConn, err := grpc.DialContext(pdCtx, pdAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	pdCancel()
	if err != nil {
		log.Fatalf("  PD (%s): FAIL - cannot connect: %v", pdAddr, err)
	}
	defer pdConn.Close()

	pdClient := pdpb.NewPDClient(pdConn)

	membersCtx, membersCancel := context.WithTimeout(context.Background(), 5*time.Second)
	membersResp, err := pdClient.GetMembers(membersCtx, &pdpb.GetMembersRequest{})
	membersCancel()
	if err != nil {
		log.Fatalf("  PD (%s): FAIL - GetMembers: %v", pdAddr, err)
	}
	fmt.Printf("  PD (%s): OK (leader=%s)\n", pdAddr, membersResp.GetLeader().GetName())
	fmt.Println()

	// Step 2: Discover leader via PD.
	fmt.Println("[2/4] Discovering leader via PD...")
	testKey := []byte("pd-cluster-verify-key")
	testValue := []byte("pd-cluster-verify-value-" + time.Now().Format("150405"))
	startTS := uint64(time.Now().UnixNano() / 1000)
	commitTS := startTS + 10

	var leaderAddr string
	var leaderStoreID uint64
	// Retry — the leader heartbeat may not have reached PD yet.
	for attempt := 0; attempt < 15; attempt++ {
		rCtx, rCancel := context.WithTimeout(context.Background(), 3*time.Second)
		regionResp, err := pdClient.GetRegion(rCtx, &pdpb.GetRegionRequest{
			Header:    &pdpb.RequestHeader{ClusterId: 1},
			RegionKey: testKey,
		})
		rCancel()
		if err == nil && regionResp.GetLeader() != nil && regionResp.GetLeader().GetStoreId() != 0 {
			leaderStoreID = regionResp.GetLeader().GetStoreId()
			sCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
			storeResp, err := pdClient.GetStore(sCtx, &pdpb.GetStoreRequest{
				Header:  &pdpb.RequestHeader{ClusterId: 1},
				StoreId: leaderStoreID,
			})
			sCancel()
			if err == nil && storeResp.GetStore() != nil && storeResp.GetStore().GetAddress() != "" {
				leaderAddr = storeResp.GetStore().GetAddress()
				break
			}
		}
		fmt.Printf("  Waiting for leader info from PD (attempt %d/15)...\n", attempt+1)
		time.Sleep(2 * time.Second)
	}
	if leaderAddr == "" {
		log.Fatal("  FAIL: could not discover leader via PD after 15 attempts")
	}
	fmt.Printf("  Leader: store %d at %s\n", leaderStoreID, leaderAddr)
	fmt.Println()

	// Step 3: Write data to the leader.
	fmt.Println("[3/4] Writing data to leader...")
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer writeCancel()

	leaderConn, err := grpc.DialContext(writeCtx, leaderAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		log.Fatalf("  FAIL: cannot connect to leader %s: %v", leaderAddr, err)
	}

	leaderClient := tikvpb.NewTikvClient(leaderConn)

	prewriteResp, err := leaderClient.KvPrewrite(writeCtx, &kvrpcpb.PrewriteRequest{
		Mutations:    []*kvrpcpb.Mutation{{Op: kvrpcpb.Op_Put, Key: testKey, Value: testValue}},
		PrimaryLock:  testKey,
		StartVersion: startTS,
		LockTtl:      5000,
	})
	if err != nil {
		leaderConn.Close()
		log.Fatalf("  FAIL: prewrite: %v", err)
	}
	if len(prewriteResp.GetErrors()) > 0 {
		leaderConn.Close()
		log.Fatalf("  FAIL: prewrite errors: %v", prewriteResp.GetErrors())
	}

	commitResp, err := leaderClient.KvCommit(writeCtx, &kvrpcpb.CommitRequest{
		Keys:          [][]byte{testKey},
		StartVersion:  startTS,
		CommitVersion: commitTS,
	})
	if err != nil {
		leaderConn.Close()
		log.Fatalf("  FAIL: commit: %v", err)
	}
	if commitResp.GetError() != nil {
		leaderConn.Close()
		log.Fatalf("  FAIL: commit error: %v", commitResp.GetError())
	}
	fmt.Printf("  Wrote key=%s value=%s via leader (store %d)\n", testKey, testValue, leaderStoreID)

	getResp, err := leaderClient.KvGet(writeCtx, &kvrpcpb.GetRequest{
		Key:     testKey,
		Version: commitTS + 10,
	})
	if err != nil || getResp.GetNotFound() {
		leaderConn.Close()
		log.Fatalf("  FAIL: read-back on leader failed")
	}
	fmt.Printf("  Verified on leader: value=%s\n", getResp.GetValue())
	leaderConn.Close()
	fmt.Println()

	// Step 4: Read from follower nodes via PD store list.
	fmt.Println("[4/4] Cross-node read verification...")
	time.Sleep(1 * time.Second)

	allCtx, allCancel := context.WithTimeout(context.Background(), 5*time.Second)
	allStoresResp, err := pdClient.GetAllStores(allCtx, &pdpb.GetAllStoresRequest{
		Header: &pdpb.RequestHeader{ClusterId: 1},
	})
	allCancel()
	if err != nil {
		log.Fatalf("  FAIL: GetAllStores: %v", err)
	}

	verified := 0
	for _, store := range allStoresResp.GetStores() {
		if store.GetId() == leaderStoreID {
			continue
		}
		addr := store.GetAddress()
		if addr == "" {
			continue
		}

		nodeCtx, nodeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := grpc.DialContext(nodeCtx, addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)
		if err != nil {
			nodeCancel()
			fmt.Printf("  Store %d (%s): SKIP - cannot connect: %v\n", store.GetId(), addr, err)
			continue
		}

		client := tikvpb.NewTikvClient(conn)
		resp, err := client.KvGet(nodeCtx, &kvrpcpb.GetRequest{
			Key:     testKey,
			Version: commitTS + 10,
		})
		conn.Close()
		nodeCancel()

		if err != nil {
			fmt.Printf("  Store %d (%s): FAIL - %v\n", store.GetId(), addr, err)
			continue
		}
		if resp.GetNotFound() {
			fmt.Printf("  Store %d (%s): FAIL - key not found\n", store.GetId(), addr)
			continue
		}
		if string(resp.GetValue()) != string(testValue) {
			fmt.Printf("  Store %d (%s): FAIL - value mismatch\n", store.GetId(), addr)
			continue
		}

		fmt.Printf("  Store %d (%s): OK - value=%s\n", store.GetId(), addr, resp.GetValue())
		verified++
	}

	fmt.Println()
	totalFollowers := len(allStoresResp.GetStores()) - 1
	if totalFollowers <= 0 {
		totalFollowers = 1
	}
	if verified >= 1 {
		fmt.Printf("=== PASS: PD cluster cross-node replication verified (%d/%d follower nodes) ===\n", verified, totalFollowers)
	} else {
		fmt.Println("=== FAIL: PD cluster cross-node replication not verified ===")
		os.Exit(1)
	}
}
