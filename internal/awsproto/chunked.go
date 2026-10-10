package awsproto

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// crc64NVME is CRC-64/NVME; Go's crc64 uses the reflected polynomial with
// all-ones init and final xor, which matches this variant.
var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// ChecksumAlgorithms maps x-amz-checksum-<name> to a hash constructor.
var ChecksumAlgorithms = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"crc64nvme": func() hash.Hash { return crc64.New(crc64NVME) },
	"sha1":      sha1.New,
	"sha256":    sha256.New,
}

// Checksum returns base64(alg(data)), the format of x-amz-checksum-* values.
func Checksum(alg string, data []byte) string {
	h := ChecksumAlgorithms[alg]()
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// IsAWSChunked reports whether the body uses the aws-chunked encoding.
func IsAWSChunked(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
		strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-")
}

// DecodeAWSChunked decodes an aws-chunked body:
//
//	<hex-size>[;chunk-signature=...]\r\n<data>\r\n ... 0[;...]\r\n[trailer: value\r\n]...\r\n
//
// It returns the payload and any trailing headers (e.g. x-amz-checksum-crc32).
func DecodeAWSChunked(body io.Reader) ([]byte, map[string]string, error) {
	br := bufio.NewReaderSize(body, 64*1024)
	var out bytes.Buffer
	trailers := map[string]string{}
	bad := func(msg string) error { return Errorf(http.StatusBadRequest, "IncompleteBody", "aws-chunked: %s", msg) }
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, nil, bad("missing chunk header")
		}
		sizeStr, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil || size < 0 {
			return nil, nil, bad("invalid chunk size " + strconv.Quote(sizeStr))
		}
		if size == 0 {
			break
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, nil, bad("truncated chunk")
		}
		if crlf, err := br.ReadString('\n'); err != nil || strings.TrimSpace(crlf) != "" {
			return nil, nil, bad("missing CRLF after chunk")
		}
	}
	// Trailers run until a blank line or EOF.
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if k, v, ok := strings.Cut(line, ":"); ok {
				trailers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		if err != nil || line == "" {
			break
		}
	}
	return out.Bytes(), trailers, nil
}

// ReadBody reads a request body, decoding aws-chunked and verifying
// Content-MD5 and any x-amz-checksum-* header or trailer. It returns the
// payload and the verified checksums keyed by algorithm.
func ReadBody(r *http.Request) ([]byte, map[string]string, error) {
	var data []byte
	sums := map[string]string{}
	if IsAWSChunked(r) {
		d, trailers, err := DecodeAWSChunked(r.Body)
		if err != nil {
			return nil, nil, err
		}
		data = d
		for k, v := range trailers {
			if alg := strings.TrimPrefix(k, "x-amz-checksum-"); alg != k {
				sums[alg] = v
			}
		}
		if dl := r.Header.Get("X-Amz-Decoded-Content-Length"); dl != "" && dl != strconv.Itoa(len(data)) {
			return nil, nil, Errorf(http.StatusBadRequest, "IncompleteBody",
				"decoded length %d does not match x-amz-decoded-content-length %s", len(data), dl)
		}
	} else {
		d, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, nil, Errorf(http.StatusBadRequest, "IncompleteBody", "%v", err)
		}
		data = d
	}
	for alg := range ChecksumAlgorithms {
		if v := r.Header.Get("X-Amz-Checksum-" + alg); v != "" {
			sums[alg] = v
		}
	}
	for alg, want := range sums {
		if _, ok := ChecksumAlgorithms[alg]; !ok {
			delete(sums, alg)
			continue
		}
		if got := Checksum(alg, data); got != want {
			return nil, nil, Errorf(http.StatusBadRequest, "BadDigest",
				"The %s you specified did not match the calculated checksum.", strings.ToUpper(alg))
		}
	}
	if want := r.Header.Get("Content-Md5"); want != "" {
		sum := md5.Sum(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != want {
			return nil, nil, Errorf(http.StatusBadRequest, "BadDigest",
				"The Content-MD5 you specified did not match what we received.")
		}
	}
	return data, sums, nil
}
