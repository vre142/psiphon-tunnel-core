package main

// #include <stdlib.h>
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
)

type startResultCode int

const (
	startResultCodeSuccess startResultCode = iota
	startResultCodeTimeout
	startResultCodeOtherError
)

type noticeEvent struct {
	Data       map[string]interface{} `json:"data"`
	NoticeType string                 `json:"noticeType"`
}

type startResult struct {
	Code           startResultCode `json:"result_code"`
	BootstrapTime  float64         `json:"bootstrap_time,omitempty"`
	ErrorString    string          `json:"error,omitempty"`
	HttpProxyPort  int             `json:"http_proxy_port,omitempty"`
	SocksProxyPort int             `json:"socks_proxy_port,omitempty"`
}

type psiphonTunnel struct {
	controllerWaitGroup sync.WaitGroup
	controllerCtx       context.Context
	stopController      context.CancelFunc
	httpProxyPort       int
	socksProxyPort      int
}

var tunnel psiphonTunnel

// Memory managed by PsiphonTunnel which is allocated in Start and freed in Stop
var managedStartResult *C.char

//export Start
//
// ******************************* WARNING ********************************
// The underlying memory referenced by the return value of Start is managed
// by PsiphonTunnel and attempting to free it explicitly will cause the
// program to crash. This memory is freed once Stop is called.
// ************************************************************************
//
// Start starts the controller and returns once either of the following has occured: an active tunnel has been
// established, the timeout has elapsed before an active tunnel could be established or an error has occured.
//
// Start returns a startResult object serialized as a JSON string in the form of a null-terminated buffer of C chars.
// Start will return,
// On success:
//   {
//     "result_code": 0,
//     "bootstrap_time": <time_to_establish_tunnel>,
//     "http_proxy_port": <http_proxy_port_num>,
//     "socks_proxy_port": <socks_proxy_port_num>
//   }
//
// On timeout:
//  {
//    "result_code": 1,
//    "error": <error message>
//  }
//
// On other error:
//   {
//     "result_code": 2,
//     "error": <error message>
//   }
//
// clientPlatform should be of the form OS_OSVersion_BundleIdentifier where both the OSVersion and BundleIdentifier
// fields are optional. If clientPlatform is set to an empty string the "ClientPlatform" field in the provided json
// config will be used instead.
//
// Provided below are links to platform specific code which can be used to find some of the above fields:
//   Android:
//     - OSVersion: https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/Android/PsiphonTunnel/PsiphonTunnel.java#L573
//     - BundleIdentifier: https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/Android/PsiphonTunnel/PsiphonTunnel.java#L575
//   iOS:
//     - OSVersion: https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/iOS/PsiphonTunnel/PsiphonTunnel/PsiphonTunnel.m#L612
//     - BundleIdentifier: https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/iOS/PsiphonTunnel/PsiphonTunnel/PsiphonTunnel.m#L622
//
// Some examples of valid client platform strings are:
//
//   "Android_4.2.2_com.example.exampleApp"
//   "iOS_11.4_com.example.exampleApp"
//   "Windows"
//
// networkID must be a non-empty string and follow the format specified by
// https://godoc.org/github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon#NetworkIDGetter.
//
// Provided below are links to platform specific code which can be used to generate valid network identifier strings:
//   Android:
//     - https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/Android/PsiphonTunnel/PsiphonTunnel.java#L371
//   iOS:
//     - https://github.com/Psiphon-Labs/psiphon-tunnel-core/blob/3d344194d21b250e0f18ededa4b4459a373b0690/MobileLibrary/iOS/PsiphonTunnel/PsiphonTunnel/PsiphonTunnel.m#L1105
func Start(configJSON, embeddedServerEntryList, clientPlatform, networkID string, timeout int64) *C.char {
	// NOTE: all arguments which are still referenced once Start returns should be copied onto the Go heap
	//       to ensure that they don't disappear later on and cause Go to crash.

	// Load provided config

	config, err := psiphon.LoadConfig([]byte(configJSON))
	if err != nil {
		return startErrorJson(err)
	}

	// Set network ID

	if networkID != "" {
		// Ensure config.NetworkID is on the Go heap
		config.NetworkID = deepCopy(networkID)
	}

	// Set client platform

	if clientPlatform != "" {
		// Ensure config.ClientPlatform is on the Go heap
		config.ClientPlatform = deepCopy(clientPlatform)
	}

	// All config fields should be set before calling commit
	err = config.Commit()
	if err != nil {
		return startErrorJson(err)
	}

	// Setup signals

	connected := make(chan bool)

	testError := make(chan error)

	// Set up notice handling

	psiphon.SetNoticeWriter(psiphon.NewNoticeReceiver(
		func(notice []byte) {

			var event noticeEvent

			err := json.Unmarshal(notice, &event)
			if err != nil {
				err = errors.New(fmt.Sprintf("Failed to unmarshal json: %s", err.Error()))
				select {
				case testError <- err:
				default:
				}
			}

			if event.NoticeType == "ListeningHttpProxyPort" {
				port := event.Data["port"].(float64)
				tunnel.httpProxyPort = int(port)
			} else if event.NoticeType == "ListeningSocksProxyPort" {
				port := event.Data["port"].(float64)
				tunnel.socksProxyPort = int(port)
			} else if event.NoticeType == "Tunnels" {
				count := event.Data["count"].(float64)
				if count > 0 {
					select {
					case connected <- true:
					default:
					}
				}
			}
		}))

	// Initialize data store

	err = psiphon.OpenDataStore(config)
	if err != nil {
		return startErrorJson(err)
	}

	// Store embedded server entries

	serverEntries, err := protocol.DecodeServerEntryList(
		embeddedServerEntryList,
		common.GetCurrentTimestamp(),
		protocol.SERVER_ENTRY_SOURCE_EMBEDDED)
	if err != nil {
		return startErrorJson(err)
	}

	err = psiphon.StoreServerEntries(config, serverEntries, false)
	if err != nil {
		return startErrorJson(err)
	}

	// Run Psiphon

	controller, err := psiphon.NewController(config)
	if err != nil {
		return startErrorJson(err)
	}

	tunnel.controllerCtx, tunnel.stopController = context.WithCancel(context.Background())

	// Set start time

	startTime := time.Now()

	// Setup timeout signal

	runtimeTimeout := time.Duration(timeout) * time.Second

	timeoutSignal, cancelTimeout := context.WithTimeout(context.Background(), runtimeTimeout)
	defer cancelTimeout()

	// Run test

	var result startResult

	tunnel.controllerWaitGroup.Add(1)
	go func() {
		defer tunnel.controllerWaitGroup.Done()
		controller.Run(tunnel.controllerCtx)

		select {
		case testError <- errors.New("controller.Run exited unexpectedly"):
		default:
		}
	}()

	// Wait for an active tunnel, timeout or error

	select {
	case <-connected:
		result.Code = startResultCodeSuccess
		result.BootstrapTime = secondsBeforeNow(startTime)
		result.HttpProxyPort = tunnel.httpProxyPort
		result.SocksProxyPort = tunnel.socksProxyPort
	case <-timeoutSignal.Done():
		result.Code = startResultCodeTimeout
		err = timeoutSignal.Err()
		if err != nil {
			result.ErrorString = fmt.Sprintf("Timeout occured before Psiphon connected: %s", err.Error())
		}
		tunnel.stopController()
	case err := <-testError:
		result.Code = startResultCodeOtherError
		result.ErrorString = err.Error()
		tunnel.stopController()
	}

	// Free previous result
	freeManagedStartResult()

	// Return result
	managedStartResult = marshalStartResult(result)

	return managedStartResult
}

//export Stop
// Stop stops the controller if it is running and waits for it to clean up and exit.
//
// Stop should always be called after a successful call to Start to ensure the
// controller is not left running.
func Stop() {
	freeManagedStartResult()

	if tunnel.stopController != nil {
		tunnel.stopController()
	}

	tunnel.controllerWaitGroup.Wait()

	psiphon.CloseDataStore()
}

// secondsBeforeNow returns the delta seconds of the current time subtract startTime.
func secondsBeforeNow(startTime time.Time) float64 {
	delta := time.Now().Sub(startTime)
	return delta.Seconds()
}

// marshalStartResult serializes a startResult object as a JSON string in the form
// of a null-terminated buffer of C chars.
func marshalStartResult(result startResult) *C.char {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return C.CString(fmt.Sprintf("{\"result_code\":%d, \"error\": \"%s\"}", startResultCodeOtherError, err.Error()))
	}

	return C.CString(string(resultJSON))
}

// startErrorJson returns a startResult object serialized as a JSON string in the form
// of a null-terminated buffer of C chars. The object's return result code will be set to
// startResultCodeOtherError (2) and its error string set to the error string of the provided error.
//
// The JSON will be in the form of:
// {
//   "result_code": 2,
//   "error": <error message>
// }
func startErrorJson(err error) *C.char {
	var result startResult
	result.Code = startResultCodeOtherError
	result.ErrorString = err.Error()

	return marshalStartResult(result)
}

// deepCopy copies a string's underlying buffer and returns a new string which references the new buffer.
func deepCopy(s string) string {
	return string([]byte(s))
}

// freeManagedStartResult frees the memory on the heap pointed to by managedStartResult.
func freeManagedStartResult() {
	if managedStartResult != nil {
		managedMemory := unsafe.Pointer(managedStartResult)
		if managedMemory != nil {
			C.free(managedMemory)
		}
		managedStartResult = nil
	}
}

// main is a stub required by cgo.
func main() {}
