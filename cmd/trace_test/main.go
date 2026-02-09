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
	fmt.Println("=== Writing 200000 keys ===")
	for i := 0; i < 200000; i++ {
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
	for i := 199500; i < 200000; i++ {
		key := fmt.Sprintf("key-%04d", i)
		val, err := db.Get([]byte(key))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s = %s \n", key, val)
	}

	if err := db.Close(); err != nil {
		log.Fatal(err)
	}

	fmt.Println("\n✓ Test completed!")
	fmt.Println("Check the global log file (geth-trace-YYYY-MM-DD-HH-MM-SS) for detailed trace data.")
	fmt.Println("All Get operations should include level and sstable metadata.")
}
