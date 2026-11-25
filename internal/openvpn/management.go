package openvpn

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// clientCounters are the cumulative byte counters the management interface
// reports for one connected client: Rx is what the server received from the
// client (the client's upload), Tx is what the server sent to the client
// (the client's download).
type clientCounters struct {
	Rx int64
	Tx int64
}

// clientListLayout describes the columns in a CSV CLIENT_LIST response. The
// management interface has emitted more than one status format over the
// lifetime of OpenVPN: some versions use a comma-separated response with a
// HEADER row, while older versions use space-separated rows. Keep the offsets
// relative to the first data column because a HEADER row may start with
// `HEADER,CLIENT_LIST` while data rows start with `CLIENT_LIST`.
type clientListLayout struct {
	name int
	rx   int
	tx   int
}

func normalizedColumn(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func csvFields(line string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(line))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	return reader.Read()
}

// clientListLayoutFromHeader recognises both
//
//	CLIENT_LIST,Common Name,...,Bytes Received,Bytes Sent,...
//
// and
//
//	HEADER,CLIENT_LIST,Common Name,...,Bytes Received,Bytes Sent,...
//
// forms used by OpenVPN status/management replies.
func clientListLayoutFromHeader(fields []string) (clientListLayout, bool) {
	base := 0
	if len(fields) > 0 && normalizedColumn(fields[0]) == "header" {
		if len(fields) < 2 || normalizedColumn(fields[1]) != "client_list" {
			return clientListLayout{}, false
		}
		base = 2
	} else if len(fields) > 0 && normalizedColumn(fields[0]) == "client_list" {
		base = 1
	}

	layout := clientListLayout{name: -1, rx: -1, tx: -1}
	for i := base; i < len(fields); i++ {
		switch normalizedColumn(fields[i]) {
		case "common name", "common_name", "cn":
			if layout.name >= 0 {
				return clientListLayout{}, false
			}
			layout.name = i - base
		case "bytes received", "bytes_received", "bytes rx", "rx":
			if layout.rx >= 0 {
				return clientListLayout{}, false
			}
			layout.rx = i - base
		case "bytes sent", "bytes_sent", "bytes tx", "tx":
			if layout.tx >= 0 {
				return clientListLayout{}, false
			}
			layout.tx = i - base
		}
	}
	if layout.name < 0 || layout.rx < 0 || layout.tx < 0 {
		return clientListLayout{}, false
	}
	return layout, true
}

const maxManagementCounter = int64(^uint64(0) >> 1)

func parseManagementCounter(raw string) (int64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, err
	}
	if value > uint64(maxManagementCounter) {
		return 0, errors.New("openvpn management counter exceeds supported range")
	}
	return int64(value), nil
}

func saturatingManagementAdd(current, delta int64) int64 {
	if delta <= 0 {
		return current
	}
	if current >= maxManagementCounter-delta {
		return maxManagementCounter
	}
	return current + delta
}

func parseCounterPair(fields []string, start int) (clientCounters, bool) {
	// Headerless formats are only safe when the expected counter columns are
	// present at their known offsets. Searching forward can reinterpret a
	// timestamp or client ID as traffic after a malformed counter.
	if start < 0 || start+1 >= len(fields) {
		return clientCounters{}, false
	}
	rx, errRx := parseManagementCounter(fields[start])
	tx, errTx := parseManagementCounter(fields[start+1])
	if errRx != nil || errTx != nil {
		return clientCounters{}, false
	}
	return clientCounters{Rx: rx, Tx: tx}, true
}

func parseCSVClientRow(fields []string, layout *clientListLayout) (string, clientCounters, bool) {
	if len(fields) == 0 {
		return "", clientCounters{}, false
	}

	// A header can be prefixed with HEADER,CLIENT_LIST, whereas a data row is
	// normally prefixed with CLIENT_LIST. Convert the relative layout to the
	// current row's base before indexing it.
	base := 0
	if normalizedColumn(fields[0]) == "client_list" {
		base = 1
	} else if normalizedColumn(fields[0]) == "header" {
		return "", clientCounters{}, false
	}
	if layout != nil {
		nameIndex := base + layout.name
		rxIndex := base + layout.rx
		txIndex := base + layout.tx
		if nameIndex >= 0 && rxIndex >= 0 && txIndex >= 0 &&
			nameIndex < len(fields) && rxIndex < len(fields) && txIndex < len(fields) {
			cn := strings.TrimSpace(fields[nameIndex])
			rx, errRx := parseManagementCounter(fields[rxIndex])
			tx, errTx := parseManagementCounter(fields[txIndex])
			if cn != "" && errRx == nil && errTx == nil && rx >= 0 && tx >= 0 {
				return cn, clientCounters{Rx: rx, Tx: tx}, true
			}
		}
		return "", clientCounters{}, false
	}

	// Traditional status output has no CLIENT_LIST prefix and places the
	// counters immediately after Common Name and Real Address. A management
	// response without a header uses the same row shape but may carry the
	// CLIENT_LIST prefix.
	cnIndex := base
	if cnIndex >= len(fields) {
		return "", clientCounters{}, false
	}
	cn := strings.TrimSpace(fields[cnIndex])
	if cn == "" {
		return "", clientCounters{}, false
	}
	start := base + 2
	counters, ok := parseCounterPair(fields, start)
	if !ok {
		return "", clientCounters{}, false
	}
	return cn, counters, true
}

func parseSpaceClientRow(line string) (string, clientCounters, bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return "", clientCounters{}, false
	}

	first := strings.TrimSpace(fields[0])
	if first != "CLIENT_LIST" && first != "<CLIENT_LIST" {
		return "", clientCounters{}, false
	}
	if strings.EqualFold(fields[1], "header") || strings.EqualFold(fields[1], "version") {
		return "", clientCounters{}, false
	}

	// `<CLIENT_LIST 1 CN ...>` includes a numeric client id; the regular
	// `CLIENT_LIST CN ...>` form does not. Common names are email addresses,
	// so a numeric token immediately after the marker is unambiguously the id.
	base := 1
	if first == "<CLIENT_LIST" {
		base = 2
	} else if _, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
		base = 2
	}
	if base >= len(fields) {
		return "", clientCounters{}, false
	}
	cn := fields[base]
	counters, ok := parseCounterPair(fields, base+3)
	if !ok || cn == "" {
		return "", clientCounters{}, false
	}
	return cn, counters, true
}

// parseClientListResponse parses one complete management response. It is kept
// separate from the socket code so the real OpenVPN response formats can be
// tested without requiring an OpenVPN daemon in CI.
// errUnsafeManagementResponse means a recognized response contained
// accounting evidence that cannot be interpreted without losing information.
// Do not hide it by trying a different command that may show another snapshot.
var errUnsafeManagementResponse = errors.New("unsafe openvpn management response")

func parseClientListResponse(reader io.Reader) (map[string]clientCounters, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	out := make(map[string]clientCounters)
	seen := make(map[string]bool)
	var layout *clientListLayout
	terminated, recognized, legacyRows, clientEvidence := false, false, false, false
	add := func(cn string, counters clientCounters) error {
		key := strings.ToLower(strings.TrimSpace(cn))
		if key == "" || seen[key] {
			return fmt.Errorf("%w: blank or duplicate client identity", errUnsafeManagementResponse)
		}
		seen[key] = true
		out[cn] = counters
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "OK" || strings.HasPrefix(line, ">INFO:") {
			continue
		}
		if line == "END" || line == "<END" {
			terminated = true
			break
		}
		if strings.HasPrefix(line, "ERROR:") {
			return nil, errors.New(strings.TrimSpace(line))
		}
		if strings.HasPrefix(line, "<CLIENT_LIST VERSION ") {
			recognized = true
			continue
		}
		if strings.HasPrefix(line, "<CLIENT_LIST HEADER ") {
			recognized = true
			if !strings.Contains(line, "Common Name") || !strings.Contains(line, "Bytes Received") || !strings.Contains(line, "Bytes Sent") {
				return nil, fmt.Errorf("%w: incomplete legacy client header", errUnsafeManagementResponse)
			}
			clientEvidence = true
			continue
		}
		if strings.HasPrefix(line, "OpenVPN CLIENT LIST") {
			recognized = true
			continue
		}
		if line == "ROUTING TABLE" || line == "GLOBAL STATS" {
			legacyRows = false
			continue
		}
		// A truncated marker is not an empty client list. TrimSpace above
		// also turns "CLIENT_LIST\t" into this exact form.
		if line == "CLIENT_LIST" || line == "<CLIENT_LIST" {
			return nil, fmt.Errorf("%w: incomplete client row marker", errUnsafeManagementResponse)
		}

		// status 3 is tab-delimited (not CSV); splitting on whitespace loses
		// empty IPv6 fields and splits column names and timestamps into tokens.
		var fields []string
		var err error
		switch i := strings.IndexAny(line, ",\t"); {
		case i >= 0 && line[i] == '\t':
			fields = strings.Split(line, "\t")
		case i >= 0:
			fields, err = csvFields(line)
		}
		if err != nil {
			if strings.HasPrefix(line, "CLIENT_LIST,") || strings.HasPrefix(line, "HEADER,CLIENT_LIST,") || legacyRows {
				return nil, fmt.Errorf("%w: malformed client row", errUnsafeManagementResponse)
			}
			continue
		}
		if len(fields) > 0 {
			first := normalizedColumn(fields[0])
			isHeader := first == "header" && len(fields) > 1 && normalizedColumn(fields[1]) == "client_list"
			isLegacyHeader := first == "common name"
			isUnprefixedHeader := first == "client_list" && len(fields) > 1 && normalizedColumn(fields[1]) == "common name"
			if isHeader || isLegacyHeader || isUnprefixedHeader {
				parsed, ok := clientListLayoutFromHeader(fields)
				if !ok {
					return nil, fmt.Errorf("%w: incomplete client header", errUnsafeManagementResponse)
				}
				layout = &parsed
				recognized = true
				clientEvidence = true
				legacyRows = isLegacyHeader
				continue
			}
			if first == "client_list" || legacyRows {
				if layout == nil {
					return nil, fmt.Errorf("%w: client row without header", errUnsafeManagementResponse)
				}
				cn, counters, ok := parseCSVClientRow(fields, layout)
				if !ok {
					return nil, fmt.Errorf("%w: invalid client row", errUnsafeManagementResponse)
				}
				if err := add(cn, counters); err != nil {
					return nil, err
				}
				continue
			}
			// Routing and global-stat rows do not describe clients.
			continue
		}
		// Preserve the older space-delimited management format.
		if strings.HasPrefix(line, "<CLIENT_LIST ") || strings.HasPrefix(line, "CLIENT_LIST ") {
			cn, counters, ok := parseSpaceClientRow(line)
			if !ok {
				return nil, fmt.Errorf("%w: invalid legacy client row", errUnsafeManagementResponse)
			}
			recognized = true
			clientEvidence = true
			if err := add(cn, counters); err != nil {
				return nil, err
			}
			continue
		}
		// While inside a traditional unprefixed client table, an unknown
		// line with no delimiters may be a truncated client record. It must
		// not be silently discarded as if the client had disconnected.
		if legacyRows {
			return nil, fmt.Errorf("%w: invalid legacy client row", errUnsafeManagementResponse)
		}
	}
	if err := scanner.Err(); err != nil {
		if recognized {
			return nil, fmt.Errorf("%w: incomplete response: %v", errUnsafeManagementResponse, err)
		}
		return nil, err
	}
	if !terminated {
		if recognized {
			return nil, fmt.Errorf("%w: response ended before END", errUnsafeManagementResponse)
		}
		return nil, errors.New("openvpn management response ended before END")
	}
	if !recognized {
		return nil, errors.New("unrecognized openvpn management response")
	}
	if !clientEvidence {
		return nil, fmt.Errorf("%w: response lacks client header or rows", errUnsafeManagementResponse)
	}
	return out, nil
}

func queryClientListCommand(mgmtPort int, command string) (map[string]clientCounters, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", mgmtPort), 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		return nil, err
	}
	return parseClientListResponse(conn)
}

// clientList queries the daemon's management interface and returns current
// connected clients keyed by common name. status 3 is the canonical tab-separated
// response on supported OpenVPN builds; status 2 is retained for older builds,
// and the dedicated CLIENT_LIST command is the final legacy fallback.
func clientList(mgmtPort int) (map[string]clientCounters, error) {
	var lastErr error
	for _, command := range []string{"status 3", "status 2", "CLIENT_LIST"} {
		clients, err := queryClientListCommand(mgmtPort, command)
		if err == nil {
			return clients, nil
		}
		if errors.Is(err, errUnsafeManagementResponse) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}
