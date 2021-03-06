package test

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/testground/sdk-go/run"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"

	"github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/protocol/beyond-bitswap/testbed/testbed/utils"
)

// IPFSTransfer data from S seeds to L leeches
func IPFSTransfer(runenv *runtime.RunEnv, initCtx *run.InitContext) error {
	// Test Parameters
	testvars := getEnvVars(runenv)

	/// --- Set up
	ctx, cancel := context.WithTimeout(context.Background(), testvars.Timeout)
	defer cancel()
	t, err := InitializeIPFSTest(ctx, runenv, testvars)
	if err != nil {
		return err
	}
	ipfsNode := t.ipfsNode
	signalAndWaitForAll := t.signalAndWaitForAll

	// Start still alive process if enabled
	t.stillAlive(runenv, testvars)

	var runNum int
	var tcpFetch int64

	// For each file found in the test
	for fIndex, f := range t.testFiles {

		// Accounts for every file that couldn't be found.
		var leechFails int64
		var rootCid cid.Cid

		// Wait for all nodes to be ready to start the run
		err = signalAndWaitForAll(fmt.Sprintf("start-file-%d", fIndex))
		if err != nil {
			return err
		}

		switch t.nodetp {
		case utils.Seed:
			err = t.addPublishFile(ctx, fIndex, f, runenv, testvars)
		case utils.Leech:
			rootCid, err = t.readFile(ctx, fIndex, runenv, testvars)
		}
		if err != nil {
			return err
		}

		runenv.RecordMessage("File injest complete...")
		// Wait for all nodes to be ready to dial
		err = signalAndWaitForAll(fmt.Sprintf("injest-complete-%d", fIndex))
		if err != nil {
			return err
		}

		if testvars.TCPEnabled {
			runenv.RecordMessage("Running TCP test...")
			switch t.nodetp {
			case utils.Seed:
				err = t.runTCPServer(ctx, fIndex, f, runenv, testvars)
			case utils.Leech:
				tcpFetch, err = t.runTCPFetch(ctx, fIndex, runenv, testvars)
			}
			if err != nil {
				return err
			}
		}

		runenv.RecordMessage("Starting IPFS Fetch...")

		for runNum = 1; runNum < testvars.RunCount+1; runNum++ {
			// Reset the timeout for each run
			ctx, cancel := context.WithTimeout(ctx, testvars.RunTimeout)
			defer cancel()

			runID := fmt.Sprintf("%d-%d", fIndex, runNum)

			// Wait for all nodes to be ready to start the run
			err = signalAndWaitForAll("start-run-" + runID)
			if err != nil {
				return err
			}

			runenv.RecordMessage("Starting run %d / %d (%d bytes)", runNum, testvars.RunCount, f.Size())

			dialed, err := t.dialFn(ctx, ipfsNode.Node.PeerHost, t.nodetp, t.peerInfos, testvars.MaxConnectionRate)
			if err != nil {
				return err
			}
			runenv.RecordMessage("%s Dialed %d other nodes:", t.nodetp.String(), len(dialed))

			// Wait for all nodes to be connected
			err = signalAndWaitForAll("connect-complete-" + runID)
			if err != nil {
				return err
			}

			/// --- Start test

			var timeToFetch int64
			if t.nodetp == utils.Leech {
				// For each wave
				for waveNum := 0; waveNum < testvars.NumWaves; waveNum++ {
					// Only leecheers for that wave entitled to leech.
					if (t.tpindex % testvars.NumWaves) == waveNum {
						runenv.RecordMessage("Starting wave %d", waveNum)
						// Stagger the start of the first request from each leech
						// Note: seq starts from 1 (not 0)
						startDelay := time.Duration(t.seq-1) * testvars.RequestStagger

						runenv.RecordMessage("Starting to leech %d / %d (%d bytes)", runNum, testvars.RunCount, f.Size())
						runenv.RecordMessage("Leech fetching data after %s delay", startDelay)
						start := time.Now()
						// TODO: Here we may be able to define requesting pattern. ipfs.DAG()
						// Right now using a path.
						fPath := path.IpfsPath(rootCid)
						runenv.RecordMessage("Got path for file: %v", fPath)
						ctxFetch, cancel := context.WithTimeout(ctx, testvars.RunTimeout/2)
						// Pin Add also traverse the whole DAG
						// err := ipfsNode.API.Pin().Add(ctxFetch, fPath)
						rcvFile, err := ipfsNode.API.Unixfs().Get(ctxFetch, fPath)
						if err != nil {
							runenv.RecordMessage("Error fetching data from IPFS: %w", err)
							leechFails++
						} else {
							err = files.WriteTo(rcvFile, "/tmp/"+strconv.Itoa(t.tpindex)+time.Now().String())
							if err != nil {
								cancel()
								return err
							}
							timeToFetch = time.Since(start).Nanoseconds()
							s, _ := rcvFile.Size()
							runenv.RecordMessage("Leech fetch of %d complete (%d ns) for wave %d", s, timeToFetch, waveNum)
						}
						cancel()
					}
					if waveNum < testvars.NumWaves-1 {
						runenv.RecordMessage("Waiting 5 seconds between waves for wave %d", waveNum)
						time.Sleep(5 * time.Second)
					}
					_, err = t.client.SignalAndWait(ctx, sync.State(fmt.Sprintf("leech-wave-%d", waveNum)), testvars.LeechCount)
				}
			}

			// Wait for all leeches to have downloaded the data from seeds
			err = signalAndWaitForAll("transfer-complete-" + runID)
			if err != nil {
				return err
			}

			/// --- Report stats
			err = ipfsNode.EmitMetrics(runenv, runNum, t.seq, t.grpseq, t.latency, t.bandwidth, int(f.Size()), t.nodetp, t.tpindex, timeToFetch, tcpFetch, leechFails, testvars.MaxConnectionRate)
			if err != nil {
				return err
			}
			runenv.RecordMessage("Finishing emitting metrics. Starting to clean...")

			err = t.cleanupRun(ctx, runenv)
			if err != nil {
				return err
			}
		}
		err = t.cleanupFile(ctx)
		if err != nil {
			return err
		}
	}

	runenv.RecordMessage("Ending testcase")
	return nil
}
