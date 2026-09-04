// Command ctxhop-installer builds and runs the CtxHop Windows installer.
//
// The release build appends a compressed ctxhop executable to a small
// Windows GUI stub. The stub remains a valid PE executable because the
// installer payload is stored after the PE image. Keeping the packer here
// avoids adding a third-party installer runtime to the release process.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	installerFormatVersion uint32 = 1
	maxInstallerPayload           = 512 << 20
	installerTrailerSize          = 8 + 4 + 8 + 8 + sha256.Size
)

var installerMagic = [8]byte{'A', 'G', 'N', 'S', 'I', 'N', 'S', 'T'}

type packOptions struct {
	stub    string
	payload string
	output  string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--pack" {
		if err := runPack(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "ctxhop-installer: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runInstaller(); err != nil {
		reportInstallerFailure(err)
		os.Exit(1)
	}
}

func runInstaller() error {
	payload, err := readInstallerPayload()
	if err != nil {
		return fmt.Errorf("read embedded CtxHop executable: %w", err)
	}
	targetPath, err := installPayload(payload)
	if err != nil {
		return fmt.Errorf("install CtxHop: %w", err)
	}
	reportInstallerSuccess(targetPath)
	return nil
}

func runPack(args []string) error {
	options, err := parsePackArgs(args)
	if err != nil {
		return err
	}

	stub, err := readPackInput(options.stub, "stub")
	if err != nil {
		return err
	}
	payload, err := readPackInput(options.payload, "payload")
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return errors.New("payload is empty")
	}
	if len(payload) > maxInstallerPayload {
		return fmt.Errorf("payload is too large (%d bytes; maximum is %d)", len(payload), maxInstallerPayload)
	}

	compressed, err := compressInstallerPayload(payload)
	if err != nil {
		return fmt.Errorf("compress payload: %w", err)
	}
	trailer := makeInstallerTrailer(payload, uint64(len(compressed)))

	output, err := filepath.Abs(options.output)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	sameStub, err := sameFilePath(options.stub, output)
	if err != nil {
		return fmt.Errorf("compare stub and output paths: %w", err)
	}
	samePayload, err := sameFilePath(options.payload, output)
	if err != nil {
		return fmt.Errorf("compare payload and output paths: %w", err)
	}
	if sameStub || samePayload {
		return errors.New("output must be different from the stub and payload")
	}
	if err := writePackedInstaller(output, stub, compressed, trailer); err != nil {
		return fmt.Errorf("write installer: %w", err)
	}
	return nil
}

func parsePackArgs(args []string) (packOptions, error) {
	flags := flag.NewFlagSet("ctxhop-installer --pack", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stub := flags.String("stub", "", "path to the platform installer stub")
	payload := flags.String("payload", "", "path to the ctxhop executable")
	output := flags.String("output", "", "path for the packed installer")
	if err := flags.Parse(args); err != nil {
		return packOptions{}, fmt.Errorf("pack: %w", err)
	}
	if flags.NArg() != 0 {
		return packOptions{}, fmt.Errorf("pack: unexpected argument %q", flags.Arg(0))
	}
	options := packOptions{
		stub:    *stub,
		payload: *payload,
		output:  *output,
	}
	for name, value := range map[string]string{
		"stub": options.stub, "payload": options.payload, "output": options.output,
	} {
		if value == "" {
			return packOptions{}, fmt.Errorf("pack: --%s is required", name)
		}
	}
	return options, nil
}

func readPackInput(path, description string) ([]byte, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s path: %w", description, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s %q: %w", description, path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s %q is a directory", description, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s %q: %w", description, path, err)
	}
	return data, nil
}

func compressInstallerPayload(payload []byte) ([]byte, error) {
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(payload); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func makeInstallerTrailer(payload []byte, compressedLength uint64) []byte {
	trailer := make([]byte, installerTrailerSize)
	copy(trailer[:len(installerMagic)], installerMagic[:])
	binary.LittleEndian.PutUint32(trailer[8:12], installerFormatVersion)
	binary.LittleEndian.PutUint64(trailer[12:20], compressedLength)
	binary.LittleEndian.PutUint64(trailer[20:28], uint64(len(payload)))
	hash := sha256.Sum256(payload)
	copy(trailer[28:], hash[:])
	return trailer
}

func writePackedInstaller(output string, stub, compressed, trailer []byte) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(output), ".ctxhop-installer-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	for _, part := range [][]byte{stub, compressed, trailer} {
		if _, err := temporary.Write(part); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("output %q already exists", output)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, output); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func readInstallerPayload() ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return readInstallerPayloadPath(executable)
}

func readInstallerPayloadPath(executable string) ([]byte, error) {
	file, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < installerTrailerSize {
		return nil, errors.New("installer payload trailer is missing")
	}
	if _, err := file.Seek(-installerTrailerSize, io.SeekEnd); err != nil {
		return nil, err
	}
	trailer := make([]byte, installerTrailerSize)
	if _, err := io.ReadFull(file, trailer); err != nil {
		return nil, err
	}
	if !bytes.Equal(trailer[:len(installerMagic)], installerMagic[:]) {
		return nil, errors.New("installer payload format is not recognized")
	}
	if version := binary.LittleEndian.Uint32(trailer[8:12]); version != installerFormatVersion {
		return nil, fmt.Errorf("unsupported installer payload version %d", version)
	}
	compressedLength := binary.LittleEndian.Uint64(trailer[12:20])
	rawLength := binary.LittleEndian.Uint64(trailer[20:28])
	if compressedLength == 0 || rawLength == 0 {
		return nil, errors.New("installer payload is empty")
	}
	if rawLength > maxInstallerPayload {
		return nil, fmt.Errorf("installer payload is too large (%d bytes)", rawLength)
	}
	if compressedLength > uint64(info.Size()) {
		return nil, errors.New("installer payload length is invalid")
	}
	payloadStart := info.Size() - installerTrailerSize - int64(compressedLength)
	if payloadStart < 0 {
		return nil, errors.New("installer payload offset is invalid")
	}
	if _, err := file.Seek(payloadStart, io.SeekStart); err != nil {
		return nil, err
	}
	compressed := io.LimitReader(file, int64(compressedLength))
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("open compressed payload: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, maxInstallerPayload+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("decompress payload: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close compressed payload: %w", closeErr)
	}
	if uint64(len(payload)) != rawLength {
		return nil, fmt.Errorf("installer payload size mismatch: got %d, expected %d", len(payload), rawLength)
	}
	hash := sha256.Sum256(payload)
	if !bytes.Equal(hash[:], trailer[28:]) {
		return nil, errors.New("installer payload checksum mismatch")
	}
	return payload, nil
}

func sameFilePath(first, second string) (bool, error) {
	first, err := filepath.Abs(first)
	if err != nil {
		return false, err
	}
	second, err = filepath.Abs(second)
	if err != nil {
		return false, err
	}
	return filepath.Clean(first) == filepath.Clean(second), nil
}
