package main

import (
	"fmt"
	"log"
	"os"

	"github.com/cockroachdb/pebble/cmd/trace_test/common"
	pebbledb "github.com/cockroachdb/pebble/cmd/trace_test/pebble"
)

func main() {
	// Initialize global log
	if !common.InitGlobalLog() {
		log.Fatal("Failed to initialize global log")
	}
	defer common.CloseGlobalLog()

	// Enable global logging
	common.SetEnableGlobalLog(true)

	dir := "/tmp/pebble_trace_test"
	os.RemoveAll(dir)

	// Use Geth wrapper (cache=128MB, handles=100, namespace="test", readonly=false, ephemeral=false)
	db, err := pebbledb.New(dir, 128, 100, "test", false, false)
	if err != nil {
		log.Fatal(err)
	}

	// 寫入一些 key-value pairs
	fmt.Println("=== Writing 10000 keys ===")
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("key-%04d", i)
		val := fmt.Sprintf("value-%04d-padding-data", i)
		if err := db.Put([]byte(key), []byte(val)); err != nil {
			log.Fatal(err)
		}
	}

	// 先 Get（此時資料在 memtable，應該看到 level=-1）
	fmt.Println("\n=== Get from memtable ===")
	val, err := db.Get([]byte("key-0005"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("key-0005 = %s (should be from memtable, level=-1)\n", val)

	// Flush 把 memtable 寫入 SSTable
	if err := db.Flush(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("\n=== Flushed to SSTable ===")

	// 再次 Get（此時資料在 L0 SSTable）
	fmt.Println("\n=== Get from L0 SSTable (first 10 keys) ===")
	for i := 9500; i < 10000; i++ {
		key := fmt.Sprintf("key-%04d", i)
		val, err := db.Get([]byte(key))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s = %s \n", key, val)
	}

	// CASTLE: probe non-existent keys that still fall within the SST's key
	// range so the bloom-reject case shows up in the trace. (Keys outside the
	// range get skipped by the manifest filter — purely in memory, no probe.)
	// Keys are inserted as "key-NNNN" (8 chars); appending "x" puts the lookup
	// strictly between two existing keys, e.g. "key-0001x" sorts after
	// "key-0001" and before "key-0002".
	fmt.Println("\n=== Get non-existent keys within SST range (bloom should reject most) ===")
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("key-%04dx", i)
		_, err := db.Get([]byte(key))
		if err != nil && err.Error() != "pebble: not found" {
			log.Fatal(err)
		}
		fmt.Printf("Get(%s) → not found (expected)\n", key)
	}

	fmt.Println("\n=== Pebble Statistics ===")
	fmt.Println(db.Stat())

	if err := db.Close(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("\n✓ Test completed!")
	fmt.Println("Check the global log file (geth-trace-YYYY-MM-DD-HH-MM-SS) for detailed trace data.")
	fmt.Println("All Get operations should include level and sstable metadata.")
}
