// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// jsonInputCfg mirrors the S3 InputSerialization.JSON block.
type jsonInputCfg struct {
	Type string // DOCUMENT | LINES
}

// jsonOutputCfg mirrors the S3 OutputSerialization.JSON block.
type jsonOutputCfg struct {
	RecordDelimiter string // default "\n"
}

// queryJSON runs expr over the JSON data in r and calls emit for each result row.
// It returns the number of input bytes scanned and output bytes produced.
func queryJSON(r io.Reader, in jsonInputCfg, out jsonOutputCfg, expr string, emit func([]byte) error) (scanned, returned int64, err error) {
	if out.RecordDelimiter == "" {
		out.RecordDelimiter = "\n"
	}

	parsed := parseSelectExpr(expr)
	parsed.outJSON = true // JSON output is always JSON for JSON input

	cr := &byteCounter{r: r}

	switch strings.ToUpper(in.Type) {
	case "DOCUMENT":
		_, returned, err = queryJSONDocument(cr, parsed, out, emit)
	default: // LINES (default)
		_, returned, err = queryJSONLines(cr, parsed, out, emit)
	}
	scanned = cr.n
	return
}

// queryJSONLines processes JSON Lines (one JSON object per line).
func queryJSONLines(r io.Reader, parsed parsedSelect, out jsonOutputCfg, emit func([]byte) error) (scanned, returned int64, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 10<<20) // 10 MiB max line
	rowIdx := int64(0)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var obj map[string]interface{}
		if jerr := json.Unmarshal(line, &obj); jerr != nil {
			continue // skip malformed lines
		}
		rowIdx++
		if parsed.limit > 0 && rowIdx > parsed.limit {
			break
		}
		row := flattenJSON(obj)
		if parsed.where != nil && !evalExpr(parsed.where, row) {
			continue
		}
		projected := projectJSONRow(parsed.cols, row, obj)
		var outLine []byte
		outLine, err = json.Marshal(projected)
		if err != nil {
			return
		}
		outLine = append(outLine, out.RecordDelimiter...)
		returned += int64(len(outLine))
		if err = emit(outLine); err != nil {
			return
		}
	}
	if serr := sc.Err(); serr != nil {
		err = serr
	}
	return
}

// queryJSONDocument treats the entire input as a single JSON value.
func queryJSONDocument(r io.Reader, parsed parsedSelect, out jsonOutputCfg, emit func([]byte) error) (scanned, returned int64, err error) {
	data, rerr := io.ReadAll(r)
	if rerr != nil {
		err = rerr
		return
	}
	scanned = int64(len(data))

	var doc interface{}
	if jerr := json.Unmarshal(data, &doc); jerr != nil {
		return
	}

	// If the document is an array, treat each element as a row.
	switch v := doc.(type) {
	case []interface{}:
		for i, elem := range v {
			if parsed.limit > 0 && int64(i+1) > parsed.limit {
				break
			}
			obj, ok := elem.(map[string]interface{})
			if !ok {
				continue
			}
			row := flattenJSON(obj)
			if parsed.where != nil && !evalExpr(parsed.where, row) {
				continue
			}
			projected := projectJSONRow(parsed.cols, row, obj)
			var outLine []byte
			outLine, err = json.Marshal(projected)
			if err != nil {
				return
			}
			outLine = append(outLine, out.RecordDelimiter...)
			returned += int64(len(outLine))
			if err = emit(outLine); err != nil {
				return
			}
		}
	case map[string]interface{}:
		row := flattenJSON(v)
		if parsed.where == nil || evalExpr(parsed.where, row) {
			projected := projectJSONRow(parsed.cols, row, v)
			var outLine []byte
			outLine, err = json.Marshal(projected)
			if err != nil {
				return
			}
			outLine = append(outLine, out.RecordDelimiter...)
			returned += int64(len(outLine))
			err = emit(outLine)
		}
	}
	return
}

// flattenJSON converts a JSON object to a string map for predicate evaluation.
// Nested values are coerced to their JSON encoding.
func flattenJSON(obj map[string]interface{}) map[string]string {
	m := make(map[string]string, len(obj))
	for k, v := range obj {
		m[k] = jsonValStr(v)
	}
	return m
}

func jsonValStr(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// projectJSONRow selects the requested columns from a JSON row.
// cols == nil means SELECT *.
func projectJSONRow(cols []selectCol, row map[string]string, obj map[string]interface{}) map[string]interface{} {
	if len(cols) == 0 {
		// SELECT * — return original obj
		out := make(map[string]interface{}, len(obj))
		for k, v := range obj {
			out[k] = v
		}
		return out
	}
	out := make(map[string]interface{}, len(cols))
	for _, c := range cols {
		src := c.src
		// strip table alias (s.name -> name)
		if dot := strings.LastIndex(src, "."); dot >= 0 {
			src = src[dot+1:]
		}
		alias := c.alias
		if alias == "" {
			alias = src
		}
		// try original object first for type fidelity
		if v, ok := obj[src]; ok {
			if c.cast != "" {
				out[alias] = castValue(jsonValStr(v), c.cast)
			} else {
				out[alias] = v
			}
		} else {
			// positional reference or fallback via row map
			val := resolveCol(c.src, row)
			if c.cast != "" {
				val = castValue(val, c.cast)
			}
			out[alias] = val
		}
	}
	return out
}
