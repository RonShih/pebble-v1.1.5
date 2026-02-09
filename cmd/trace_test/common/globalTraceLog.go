package common

import (
	"fmt"
	syslog "log"
	"os"
	"syscall"
	"time"
)

// Tino: global logger for trace collection
var gethLogger *syslog.Logger
var logFile *os.File
var targetStartBlockNumber uint64 = 100000 // The start block number for trace collection
var targetEndBlockNumber uint64 = 200000   // The end block number for trace collection
var shouldGlobalLogInUse bool = false // Flag to enable or disable global logging, it will be set to true when the target start block number is reached

var logIsInitiated bool = false

func GetTargetStartBlockNumber() uint64 {
	return targetStartBlockNumber
}

func GetTargetEndBlockNumber() uint64 {
	return targetEndBlockNumber
}

func SetEnableGlobalLog(enable bool) {
	shouldGlobalLogInUse = enable
	if !enable {
		fmt.Println("Global log is disabled.")
	} else {
		fmt.Println("Global log is enabled.")
	}
}

func WriteGlobalLog(msg string) {
	if shouldGlobalLogInUse {
		if logIsInitiated && gethLogger != nil {
			gethLogger.Println(msg)
		}
	}
}

func InitGlobalLog() bool {
	currentLogTime := time.Now().Format("2006-01-02-15-04-05")
	currentLogFileName := "./geth-trace-" + currentLogTime

	file, err := os.OpenFile(currentLogFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Println("Error opening global log file:", err)
		logIsInitiated = false
		return false
	}
	logFile = file
	gethLogger = syslog.New(file, "geth: ", syslog.Lshortfile|syslog.Ldate|syslog.Ltime)
	fmt.Println("Global log file opened successfully")
	logIsInitiated = true
	WriteGlobalLog("Global log file opened successfully")
	return true
}

func CloseGlobalLog() {
	if logFile != nil {
		logFile.Close()
		fmt.Println("Global log file closed")
	}
}

func StopChainManually() {
	pid := os.Getpid()
	fmt.Printf("Current process PID: %d\n", pid)
	err := syscall.Kill(pid, syscall.SIGINT)
	if err != nil {
		fmt.Println("Failed to send SIGINT:", err)
		return
	}
	time.Sleep(2 * time.Second)
	fmt.Println("SIGINT sent. Process should be interrupted if it handles SIGINT.")
}
