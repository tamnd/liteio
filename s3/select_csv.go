// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// csvInputCfg mirrors the S3 InputSerialization.CSV block.
type csvInputCfg struct {
	FileHeaderInfo  string // USE | IGNORE | NONE
	RecordDelimiter string // default "\n"
	FieldDelimiter  string // default ","
	QuoteCharacter  string // default `"`
	EscapeCharacter string // default `\`
}

// csvOutputCfg mirrors the S3 OutputSerialization.CSV block.
type csvOutputCfg struct {
	RecordDelimiter string // default "\n"
	FieldDelimiter  string // default ","
	QuoteCharacter  string // default `"`
}

// queryCSV runs expr over the CSV data in r and calls emit for each output row.
// The rows passed to emit are already formatted (CSV or JSON) according to outFmt
// ("CSV" or "JSON"). The function returns the number of input bytes scanned and
// output bytes produced.
func queryCSV(r io.Reader, in csvInputCfg, out csvOutputCfg, expr string, emit func([]byte) error) (scanned, returned int64, err error) {
	// Apply defaults.
	if in.FieldDelimiter == "" {
		in.FieldDelimiter = ","
	}
	if in.RecordDelimiter == "" {
		in.RecordDelimiter = "\n"
	}
	if in.QuoteCharacter == "" {
		in.QuoteCharacter = `"`
	}
	if out.FieldDelimiter == "" {
		out.FieldDelimiter = ","
	}
	if out.RecordDelimiter == "" {
		out.RecordDelimiter = "\n"
	}
	if out.QuoteCharacter == "" {
		out.QuoteCharacter = `"`
	}

	// Count bytes read via a byte-counting reader wrapper.
	bc := &byteCounter{r: r}
	csvr := csv.NewReader(bc)
	if len(in.FieldDelimiter) > 0 {
		csvr.Comma = rune(in.FieldDelimiter[0])
	}
	if len(in.QuoteCharacter) > 0 {
		// encoding/csv always uses '"'; we only support the default.
	}
	csvr.LazyQuotes = true
	csvr.FieldsPerRecord = -1 // variable

	parsed := parseSelectExpr(expr)

	var headers []string
	rowIdx := 0

	for {
		record, rerr := csvr.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// skip malformed rows
			continue
		}

		// First row: headers.
		if rowIdx == 0 {
			switch strings.ToUpper(in.FileHeaderInfo) {
			case "USE":
				headers = record
				rowIdx++
				continue
			case "IGNORE":
				rowIdx++
				// fall through: treat row as data but discard it
				continue
			default: // NONE
				// no header row; columns are _1, _2, ...
			}
		}
		rowIdx++

		// Apply LIMIT.
		if parsed.limit > 0 && int64(rowIdx-1) > parsed.limit {
			break
		}

		// Build column map.
		row := makeCSVRow(record, headers)

		// Evaluate WHERE.
		if parsed.where != nil && !evalExpr(parsed.where, row) {
			continue
		}

		// Project columns.
		projected := projectRow(parsed.cols, row, record, headers)

		// Format output.
		var line []byte
		if parsed.outJSON {
			line, err = json.Marshal(projected)
			if err != nil {
				return
			}
			line = append(line, '\n')
		} else {
			line = formatCSVRow(projected, out)
		}

		returned += int64(len(line))
		if err = emit(line); err != nil {
			return
		}
	}
	scanned = bc.n
	return
}

// makeCSVRow builds a string-keyed map from a CSV record.
// Keys are either the header names (if non-nil) or positional _1, _2, ...
func makeCSVRow(record, headers []string) map[string]string {
	m := make(map[string]string, len(record))
	for i, v := range record {
		m["_"+strconv.Itoa(i+1)] = v
		if i < len(headers) {
			m[headers[i]] = v
		}
	}
	return m
}

// projectRow returns the selected columns as an ordered map (for JSON) or slice
// (for CSV). For SELECT * it returns all columns preserving original order.
func projectRow(cols []selectCol, row map[string]string, record, headers []string) map[string]string {
	if len(cols) == 0 {
		// SELECT * — return all columns by name.
		out := make(map[string]string, len(record))
		for i, v := range record {
			key := "_" + strconv.Itoa(i+1)
			if i < len(headers) {
				key = headers[i]
			}
			out[key] = v
		}
		return out
	}
	out := make(map[string]string, len(cols))
	for _, c := range cols {
		val := resolveCol(c.src, row)
		if c.cast != "" {
			val = castValue(val, c.cast)
		}
		key := c.alias
		if key == "" {
			key = c.src
		}
		out[key] = val
	}
	return out
}

// formatCSVRow renders a projected row as a CSV line.
func formatCSVRow(row map[string]string, cfg csvOutputCfg) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if len(cfg.FieldDelimiter) > 0 {
		w.Comma = rune(cfg.FieldDelimiter[0])
	}
	// Collect values in consistent order (sorted by key).
	keys := sortedKeys(row)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = row[k]
	}
	_ = w.Write(vals)
	w.Flush()
	b := buf.Bytes()
	// Replace the trailing \n with the configured record delimiter.
	if cfg.RecordDelimiter != "\n" && len(b) > 0 && b[len(b)-1] == '\n' {
		b = append(b[:len(b)-1], cfg.RecordDelimiter...)
	}
	return b
}

// sortedKeys returns map keys in ASCII sort order for deterministic output.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort (rows are small)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// resolveCol resolves a column reference from the row map.
// It looks up the name directly, then tries positional (_N) form.
func resolveCol(ref string, row map[string]string) string {
	if v, ok := row[ref]; ok {
		return v
	}
	// try without the "s." prefix that S3 SQL uses (s._1, s.name, ...)
	if dot := strings.LastIndex(ref, "."); dot >= 0 {
		bare := ref[dot+1:]
		if v, ok := row[bare]; ok {
			return v
		}
	}
	return ""
}

// castValue applies a CAST(x AS type) conversion.
func castValue(val, typ string) string {
	switch strings.ToUpper(typ) {
	case "INT", "INTEGER":
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return val
		}
		return strconv.FormatInt(n, 10)
	case "FLOAT", "DOUBLE":
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return val
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case "BOOL", "BOOLEAN":
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "true", "1", "yes":
			return "true"
		default:
			return "false"
		}
	default: // STRING, CHAR, VARCHAR, ...
		return val
	}
}
