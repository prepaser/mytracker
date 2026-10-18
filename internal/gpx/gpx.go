package gpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prepaser/mytracker/internal/canonical"
	"github.com/prepaser/mytracker/internal/store"
)

type Point struct {
	Lat, Lon float64
	Tst      int64
	Ele      *float64
}

type ImportResult struct {
	Imported int
	Skipped  int
}

var ErrDuplicateTimestamp = errors.New("duplicate GPX timestamp")

var ErrXMLTooLarge = errors.New("GPX XML exceeds size limit")
var ErrXMLTooDeep = errors.New("GPX XML nesting exceeds depth limit")

const (
	maxXMLTokenBytes   = 64 << 10
	maxTrackPointBytes = 256 << 10
	maxXMLDepth        = 1024
)

type budgetReader struct {
	r         io.Reader
	remaining int
}

func (r *budgetReader) reset(limit int) { r.remaining = limit }

func (r *budgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		return 0, ErrXMLTooLarge
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("invalid reader count")
	}
	r.remaining -= n
	return n, err
}

func (r *budgetReader) ReadByte() (byte, error) {
	if r.remaining == 0 {
		return 0, ErrXMLTooLarge
	}
	if br, ok := r.r.(io.ByteReader); ok {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		r.remaining--
		return b, nil
	}
	var p [1]byte
	n, err := r.r.Read(p[:])
	if n == 1 {
		r.remaining--
		return p[0], nil
	}
	if n < 0 || n > 1 {
		return 0, fmt.Errorf("invalid reader count")
	}
	if err == nil {
		return 0, io.ErrNoProgress
	}
	return 0, err
}

func Parse(r io.Reader) ([]Point, error) {
	seen := map[int64]bool{}
	var out []Point
	err := Scan(r, func(p Point) error {
		if seen[p.Tst] {
			return fmt.Errorf("%w %d", ErrDuplicateTimestamp, p.Tst)
		}
		seen[p.Tst] = true
		out = append(out, p)
		return nil
	})
	return out, err
}

func Scan(r io.Reader, fn func(Point) error) error {
	if _, ok := r.(io.ByteReader); !ok {
		r = bufio.NewReaderSize(r, 4<<10)
	}
	br := &budgetReader{r: r}
	d := xml.NewDecoder(br)
	count := 0
	depth := 0
	for {
		br.reset(maxXMLTokenBytes)
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid GPX: %w", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxXMLDepth {
				return fmt.Errorf("invalid GPX: %w", ErrXMLTooDeep)
			}
			if tok.Name.Local != "trkpt" {
				continue
			}
			start := tok
			var x struct {
				Lat  string   `xml:"lat,attr"`
				Lon  string   `xml:"lon,attr"`
				Ele  *float64 `xml:"ele"`
				Time string   `xml:"time"`
			}
			br.reset(maxTrackPointBytes)
			if err := d.DecodeElement(&x, &start); err != nil {
				return fmt.Errorf("invalid track point: %w", err)
			}
			depth--
			lat, e1 := strconv.ParseFloat(x.Lat, 64)
			lon, e2 := strconv.ParseFloat(x.Lon, 64)
			when, e3 := time.Parse(time.RFC3339, x.Time)
			if e1 != nil || e2 != nil || e3 != nil || math.IsNaN(lat) || math.IsNaN(lon) || math.IsInf(lat, 0) || math.IsInf(lon, 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
				return fmt.Errorf("track point has invalid coordinates or time")
			}
			if err := fn(Point{Lat: lat, Lon: lon, Tst: when.Unix(), Ele: x.Ele}); err != nil {
				return err
			}
			count++
		case xml.EndElement:
			depth--
		}
	}
	if count == 0 {
		return fmt.Errorf("GPX contains no track points")
	}
	return nil
}

func Location(p Point, user, device string) (*store.Location, error) {
	if err := ValidateIdentity(user, device); err != nil {
		return nil, err
	}
	raw := map[string]any{"_type": "location", "tst": p.Tst, "lat": p.Lat, "lon": p.Lon}
	if p.Ele != nil {
		raw["alt"] = *p.Ele
	}
	rec, err := canonical.FromLocation(user, device, raw)
	if err != nil {
		return nil, err
	}
	digest, err := rec.Digest()
	if err != nil {
		return nil, err
	}
	payload, err := canonicalPayload(raw)
	if err != nil {
		return nil, err
	}
	return &store.Location{User: user, Device: device, Tst: p.Tst, Lat: p.Lat, Lon: p.Lon, Payload: payload, Digest: digest[:], TsaStatus: "pending", OtsStatus: "pending", CreatedAt: time.Now().Unix()}, nil
}

func ImportContext(ctx context.Context, r io.Reader, st *store.Store, user, device string, maxAttempts int) (result ImportResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ValidateIdentity(user, device); err != nil {
		return result, err
	}
	err = st.ImportAtomicContext(ctx, func(tx *store.ImportTx) error {
		return Scan(r, func(p Point) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			fresh, err := tx.MarkImportTimestamp(p.Tst)
			if err != nil {
				return err
			}
			if !fresh {
				return fmt.Errorf("%w %d", ErrDuplicateTimestamp, p.Tst)
			}
			loc, err := Location(p, user, device)
			if err != nil {
				return err
			}
			if _, err := tx.InsertLocation(loc, maxAttempts); errors.Is(err, store.ErrDuplicate) {
				result.Skipped++
				return nil
			} else if err != nil {
				return err
			}
			result.Imported++
			return nil
		})
	})
	if err != nil {
		return ImportResult{}, err
	}
	return result, nil
}

func ValidateIdentity(user, device string) error {
	for _, value := range []string{user, device} {
		if value == "" || len(value) > 128 || value != strings.TrimSpace(value) ||
			!utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("user and device must contain 1 to 128 UTF-8 bytes without surrounding whitespace or control characters")
		}
	}
	return nil
}

func canonicalPayload(v any) ([]byte, error) { return json.Marshal(v) }

func Write(w io.Writer, name string, locs func(func([]store.Location) error) error, gap time.Duration) (err error) {
	b := bufio.NewWriter(w)
	defer func() {
		if flushErr := b.Flush(); err == nil {
			err = flushErr
		}
	}()
	if _, err := fmt.Fprintln(b, `<?xml version="1.0" encoding="UTF-8"?>`); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(b, `<gpx version="1.1" creator="mytracker" xmlns="http://www.topografix.com/GPX/1/1"><trk><name>`+xmlEscape(name)+`</name><trkseg>`); err != nil {
		return err
	}
	var user, device string
	var last int64
	first := true
	err = locs(func(batch []store.Location) error {
		for _, v := range batch {
			if !first && (v.User != user || v.Device != device || v.Tst-last > int64(gap.Seconds())) {
				if _, e := fmt.Fprint(b, "</trkseg><trkseg>"); e != nil {
					return e
				}
			}
			if _, e := fmt.Fprintf(b, "<trkpt lat=\"%.7f\" lon=\"%.7f\">", v.Lat, v.Lon); e != nil {
				return e
			}
			var raw struct {
				Alt *float64 `json:"alt"`
			}
			_ = json.Unmarshal(v.Payload, &raw)
			if raw.Alt != nil {
				if _, e := fmt.Fprintf(b, "<ele>%.2f</ele>", *raw.Alt); e != nil {
					return e
				}
			}
			if _, e := fmt.Fprintf(b, "<time>%s</time></trkpt>", time.Unix(v.Tst, 0).UTC().Format(time.RFC3339)); e != nil {
				return e
			}
			user, device, last, first = v.User, v.Device, v.Tst, false
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(b, `</trkseg></trk></gpx>`)
	return err
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
