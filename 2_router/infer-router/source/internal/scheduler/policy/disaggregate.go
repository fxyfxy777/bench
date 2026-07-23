package policy

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/yzx/rl-router/internal/domain"
)

// BuildDisaggregateInfo constructs the disaggregate_info structure for PD communication.
// It determines the transfer protocol (IPC or RDMA) and provides connection details.
func BuildDisaggregateInfo(prefill, decode *domain.Instance) (map[string]any, error) {
	if prefill == nil || decode == nil {
		return nil, fmt.Errorf("prefill or decode instance is nil")
	}

	prefillHost := prefill.Host
	decodeHost := decode.Host

	// Check if IPC can be used:
	// 1. Same node (same host IP)
	// 2. Both support IPC protocol
	// 3. TP size matches (or decode tp_size is 1)
	isSameNode := prefillHost != "" && prefillHost == decodeHost
	isSupportIPC := slices.Contains(prefill.TransferProtocol, "ipc") &&
		slices.Contains(decode.TransferProtocol, "ipc")
	tpPrefill := tpSizeFromInstance(prefill)
	tpDecode := tpSizeFromInstance(decode)
	isSameTpSize := tpPrefill == tpDecode || tpDecode == 1
	useIPC := isSameNode && isSupportIPC && isSameTpSize

	transferProto := "rdma"
	if useIPC {
		transferProto = "ipc"
	}

	disagg := map[string]any{
		"prefill_ip":             prefillHost,
		"decode_ip":              decodeHost,
		"prefill_connector_port": portStringToInt(prefill.ConnectorPort),
		"decode_connector_port":  portStringToInt(decode.ConnectorPort),
		"decode_device_ids":      decode.DeviceIDs,
		"decode_rdma_ports":      decode.RDMAPorts,
		"transfer_protocol":      transferProto,
		"decode_tp_size":         tpDecode,
	}

	// Ensure slice fields are not nil in JSON output.
	if disagg["decode_device_ids"] == nil {
		disagg["decode_device_ids"] = []string{}
	}
	if disagg["decode_rdma_ports"] == nil {
		disagg["decode_rdma_ports"] = []string{}
	}

	return disagg, nil
}

// tpSizeFromInstance returns tp_size from the instance, falls back to len(DeviceIDs).
func tpSizeFromInstance(inst *domain.Instance) int {
	if inst == nil {
		return 0
	}
	if inst.TpSize > 0 {
		return inst.TpSize
	}
	if len(inst.DeviceIDs) > 0 {
		return len(inst.DeviceIDs)
	}
	return 1
}

// portStringToInt converts a port string to int.
func portStringToInt(p string) int {
	if p == "" {
		return 0
	}
	i, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return i
}

// HostFromURL extracts host part (without port) from a URL string.
func HostFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
