package structure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/fatih/color"
)

// DefaultTemplate is used when no template is given.
const DefaultTemplate = `{{if .Timestamp}}[{{.Timestamp.Format "2006-01-02 15:04:05"}}] {{else if .RawTimestamp}}[{{.RawTimestamp}}] {{end}}{{if .Severity}}{{.Severity}}: {{end}}{{.Message}}`

var severityMapping = map[string]string{
	"10":   "TRACE",
	"20":   "DEBUG",
	"30":   "INFO",
	"40":   "WARNING",
	"WARN": "WARNING",
	"50":   "ERROR",
	"60":   "FATAL",
}

var defaultExcludes = []string{
	"@timestamp", "hostname", "level", "message", "msg", "name", "pid", "severity", "text", "time", "timestamp", "ts", "v",
}

var defaultObjFields = []string{"record"}

// NewLine contains ['\n']
var NewLine = []byte("\n")

// Formatter is the system that outputs a sturctured log entry as a nice
// readable line using  a small go template (which could be given via the cli)
type Formatter struct {
	output   io.Writer
	template *template.Template

	Colorize       bool
	ShowFields     bool
	MaxFieldLength int
	ShowPrefix     bool
	ShowSuffix     bool
	IncludeFields  string
	ExcludeFields  []string
	ObjFields      []string
}

// NewFormatter compiles the given fmt as a go template and returns a Formatter
func NewFormatter(w io.Writer, fmt string) (*Formatter, error) {
	if fmt == "" {
		fmt = DefaultTemplate
	}
	tmpl, err := template.New("out").Parse(fmt)
	if err != nil {
		return nil, err
	}

	return &Formatter{
		output:         w,
		template:       tmpl,
		Colorize:       false,
		ShowFields:     true,
		MaxFieldLength: 30,
		ShowPrefix:     true,
		ShowSuffix:     true,
		IncludeFields:  "",
		ExcludeFields:  defaultExcludes,
		ObjFields:      defaultObjFields,
	}, nil
}

// Format takes a structured log entry and formats it according the template.
func (f *Formatter) Format(entry *Entry, raw json.RawMessage, prefix, suffix []byte) error {
	color.NoColor = !f.Colorize
	f.enhance(entry)

	err := f.outputSimple(prefix, f.ShowPrefix)
	if err != nil {
		return err
	}

	err = f.template.Execute(f.output, entry)
	if err != nil {
		return err
	}

	trailerJSON, trailerMultiline := f.outputFields(entry, raw)

	err = f.outputSimple(suffix, f.ShowSuffix)
	if err != nil {
		return err
	}

	err = stacktrace(f.output, raw)
	if err != nil {
		return err
	}

	_, err = f.output.Write(NewLine)
	if err != nil {
		return err
	}

	if trailerMultiline != "" {
		f.output.Write([]byte{'\t'})
		f.output.Write(bytes.ReplaceAll([]byte(trailerMultiline), []byte("\n"), []byte("\n\t")))
		f.output.Write(NewLine)
	}
	if trailerJSON != nil {
		enc := json.NewEncoder(f.output)
		enc.SetEscapeHTML(false)
		enc.SetIndent("\t", "\t")
		f.output.Write([]byte("\t"))
		enc.Encode(trailerJSON)
	}

	return nil
}

func (f *Formatter) enhance(entry *Entry) {
	if entry.Timestamp != nil && entry.Timestamp.IsZero() {
		entry.Timestamp = nil
	}

	if entry.Timestamp != nil && entry.Timestamp.Year() > 3000 { // timestamp was probably in milliseconds
		t := *entry.Timestamp
		t = time.Unix(t.Unix()/int64(time.Second/time.Millisecond), 0).UTC()
		entry.Timestamp = &t
	}

	entry.Severity = strings.ToUpper(entry.Severity)
	if level, ok := severityMapping[entry.Severity]; ok {
		entry.Severity = level
	}
	if entry.Severity != "" {
		padding := 7 - len(entry.Severity)
		if color, ok := severityColors[entry.Severity]; ok {
			entry.Severity = color(entry.Severity)
		}
		if padding > 0 {
			entry.Severity = strings.Repeat(" ", padding) + entry.Severity
		}
	}

	entry.Message = messageColor(entry.Message)
}

func (f *Formatter) outputSimple(txt []byte, toggle bool) error {
	if toggle && txt != nil && len(txt) > 0 {
		_, err := f.output.Write(txt)
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *Formatter) outputFields(entry *Entry, raw json.RawMessage) (map[string]any, string) {
	if !f.ShowFields {
		return nil, ""
	}
	fields := make(map[string]interface{})
	err := json.Unmarshal(raw, &fields)

	if labels, ok := fields["labels"]; ok {
		if labelmap, ok := labels.(map[string]interface{}); ok {
			for k, v := range labelmap {
				fields[k] = v
			}
		}
		delete(fields, "labels")
	}

	output := make([]string, 0)
	var trailerJSON map[string]interface{}
	var trailerMultiline string
	if err == nil {
		path := ""
		for key, value := range f.walkFields(fields, "") {
			if contains(f.ObjFields, key) {
				switch value := value.(type) {
				case map[string]interface{}:
					trailerJSON = value
					continue
				case string:
					trailerMultiline = value
					continue
				}
			}
			if _, ok := value.([]interface{}); ok {
				continue
			}
			if !f.shouldSkipField(key, path+"."+key, value) {
				switch v := value.(type) {
				case float64:
					output = append(output, key+"="+strconv.FormatFloat(v, 'f', -1, 64))
				default:
					output = append(output, fmt.Sprintf("%s=%v", key, value))
				}
			}
		}
		if len(output) > 0 {
			sort.Strings(output)
			fmt.Fprintf(f.output, " %v", output)
		}
	}
	return trailerJSON, trailerMultiline
}

func (f *Formatter) shouldSkipField(field, path string, value interface{}) bool {
	if strings.Contains(f.IncludeFields, field) || strings.Contains(f.IncludeFields, path) {
		return false
	}
	if strings.Count(path, ".") > 1 { // Only include nested fields when the are in the IncludeFields
		return true
	}
	if f.MaxFieldLength > 0 && len(path+fmt.Sprintf("%v", value)) >= f.MaxFieldLength {
		return true
	}

	return contains(f.ExcludeFields, field)
}

func contains(lst []string, val string) bool {
	for _, i := range lst {
		if strings.EqualFold(i, val) {
			return true
		}
	}
	return false
}

func (f *Formatter) walkFields(fields map[string]interface{}, path string) map[string]interface{} {
	result := make(map[string]interface{})
	for key, value := range fields {
		if path != "" {
			key = path + "." + key
		} else if contains(f.ObjFields, key) {
			result[key] = value
			continue
		}
		if nested, ok := value.(map[string]interface{}); ok {
			for k, v := range f.walkFields(nested, key) {
				result[k] = v
			}
		} else {
			result[key] = value
		}
	}
	return result
}
