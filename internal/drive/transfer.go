package drive

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const uploadPartSize int64 = 10 * 1024 * 1024
const storageReferer = "https://www.aliyundrive.com/"
const storageUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.159 Safari/537.36"

func canceled(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return E("TIMEOUT", "the command deadline expired", "Increase --timeout or retry the operation if needed.", 8)
	}
	if errors.Is(err, context.Canceled) {
		return E("CANCELED", "operation was canceled", "Retry the operation if needed.", 8)
	}
	return err
}

func localError(message string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return canceled(err)
	}
	return E("LOCAL_IO_ERROR", message, "Check local paths, permissions, and available disk space.", 7)
}

func storageError(ctx context.Context, message string) error {
	if ctx.Err() != nil {
		return canceled(ctx.Err())
	}
	e := E("STORAGE_ERROR", message, "Retry the transfer or check the network.", 6)
	e.Retryable = true
	return e
}

func storageResponseError(ctx context.Context, response *http.Response) error {
	if ctx.Err() != nil {
		return canceled(ctx.Err())
	}
	message := fmt.Sprintf("storage request returned HTTP %d", response.StatusCode)
	var body struct {
		Code string `xml:"Code"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err == nil && xml.Unmarshal(data, &body) == nil {
		// Only known diagnostic names are safe to expose; arbitrary upstream text is not.
		switch body.Code {
		case "AccessDenied", "AccessForbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch",
			"SecurityTokenExpired", "InvalidSecurityToken", "RequestTimeTooSkewed", "RequestHasExpired",
			"NoSuchBucket", "NoSuchKey", "NoSuchUpload", "InvalidArgument", "InvalidRequest",
			"EntityTooLarge", "EntityTooSmall", "RequestTimeout", "InternalError", "ServiceUnavailable",
			"SlowDown", "PreconditionFailed", "InvalidPart", "InvalidPartOrder", "InvalidDigest",
			"BadDigest", "SignatureVersionNotSupported", "InvalidObjectState", "MalformedXML", "TooManyRequests":
			message += " (OSS " + body.Code + ")"
		}
	}
	if ctx.Err() != nil {
		return canceled(ctx.Err())
	}
	e := E("STORAGE_ERROR", message, "Check signed storage authorization or retry the transfer.", 6)
	e.Retryable = response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return e
}

// Storage requests deliberately use no API credentials or cookie jar, including on redirects.
func (c *Client) storageClient() *http.Client {
	client := *c.HTTP
	client.Jar = nil
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many storage redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return errors.New("invalid storage redirect")
		}
		if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("insecure storage redirect")
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		// Never turn a signed storage URL into a redirect Referer.
		req.Header.Del("Referer")
		if len(via) > 0 && via[0].Header.Get("Referer") == storageReferer {
			req.Header.Set("Referer", storageReferer)
		}
		return nil
	}
	return &client
}

func storageURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return upstream("service returned an invalid storage URL")
	}
	return nil
}

type uploadPart struct {
	PartNumber int    `json:"part_number"`
	UploadURL  string `json:"upload_url,omitempty"`
}

func uploadParts(size int64) (int64, []uploadPart) {
	partSize := uploadPartSize
	if needed := size/10000 + boolInt(size%10000 != 0); needed > partSize {
		partSize = needed
	}
	count := size/partSize + boolInt(size%partSize != 0)
	if count == 0 {
		count = 1
	}
	parts := make([]uploadPart, int(count))
	for i := range parts {
		parts[i].PartNumber = i + 1
	}
	return partSize, parts
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (c *Client) Upload(ctx context.Context, local, parent, name string, dry bool) (result Transfer, err error) {
	if err := ctx.Err(); err != nil {
		return result, canceled(err)
	}
	abs, err := filepath.Abs(local)
	if err != nil {
		return result, localError("cannot resolve local upload path", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return result, localError("cannot inspect local upload file", err)
	}
	// Opening a FIFO can wait forever for a writer, even after the context expires.
	if !info.Mode().IsRegular() {
		return result, Invalid("upload supports regular files only; recursive folder upload is not supported")
	}
	if err := ctx.Err(); err != nil {
		return result, canceled(err)
	}
	input, err := os.Open(abs)
	if err != nil {
		return result, localError("cannot open local upload file", err)
	}
	defer input.Close()
	info, err = input.Stat()
	if err != nil {
		return result, localError("cannot inspect local upload file", err)
	}
	if !info.Mode().IsRegular() {
		return result, Invalid("upload supports regular files only; recursive folder upload is not supported")
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	if err := validateName(name); err != nil {
		return result, err
	}
	folder, err := c.Resolve(ctx, parent)
	if err != nil {
		return result, err
	}
	if folder.Type != "folder" {
		return result, Invalid("upload destination must be a folder")
	}
	if err := c.checkNameFree(ctx, folder.FileID, name, ""); err != nil {
		return result, err
	}
	file := File{DriveID: folder.DriveID, ParentFileID: folder.FileID, Name: name, Type: "file", Size: info.Size()}
	result = Transfer{Operation: "upload", DryRun: dry, File: file, LocalPath: abs, Bytes: info.Size()}
	if dry {
		return result, nil
	}
	partSize, parts := uploadParts(info.Size())
	var created struct {
		File
		UploadID     string       `json:"upload_id"`
		PartInfoList []uploadPart `json:"part_info_list"`
		RapidUpload  bool         `json:"rapid_upload"`
		Exist        bool         `json:"exist"`
	}
	if err := c.Request(ctx, "/v2/file/create_with_proof", map[string]any{
		"drive_id": folder.DriveID, "parent_file_id": folder.FileID, "name": name, "type": "file",
		"check_name_mode": "refuse", "size": info.Size(), "part_info_list": parts,
	}, &created); err != nil {
		return result, err
	}
	if !validID(created.FileID) || created.FileID == "root" {
		return result, upstream("upload creation response did not include a file ID")
	}
	if created.Exist {
		return result, conflict("the service reported that the upload destination already exists")
	}
	file.FileID = created.FileID
	result.File = file
	finished := false
	completionAttempted := false
	defer func() {
		if finished || err == nil || created.RapidUpload {
			return
		}
		cleaned := false
		// A lost completion response is ambiguous: never trash a potentially committed file.
		if !completionAttempted && created.UploadID != "" && ctx.Err() == nil {
			cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			cleanupErr := c.Request(cleanupCtx, "/v2/recyclebin/trash", map[string]any{"drive_id": folder.DriveID, "file_id": created.FileID}, nil)
			cancel()
			cleaned = cleanupErr == nil
		}
		var typed *Error
		if errors.As(err, &typed) {
			copy := *typed
			if cleaned {
				copy.Hint = "The incomplete upload was moved to the recycle bin. Retry the upload."
			} else {
				copy.Hint = "The upload may still exist. Inspect id:" + created.FileID + " before retrying; no cleanup was confirmed."
			}
			err = &copy
		}
	}()
	if created.RapidUpload {
		finished = true
		return result, nil
	}
	if created.UploadID == "" || (len(created.PartInfoList) != len(parts) && !(info.Size() == 0 && len(created.PartInfoList) == 0)) {
		return result, upstream("service returned an invalid multipart upload plan")
	}
	storage := c.storageClient()
	seen := map[int]bool{}
	for _, part := range created.PartInfoList {
		if part.PartNumber < 1 || part.PartNumber > len(parts) || seen[part.PartNumber] {
			return result, upstream("service returned invalid multipart part numbers")
		}
		seen[part.PartNumber] = true
		if err := storageURL(part.UploadURL); err != nil {
			return result, err
		}
		offset := int64(part.PartNumber-1) * partSize
		length := min(partSize, info.Size()-offset)
		var body io.Reader = io.NewSectionReader(input, offset, length)
		if length == 0 {
			body = http.NoBody
		}
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPut, part.UploadURL, body)
		if requestErr != nil {
			return result, storageError(ctx, "cannot construct storage upload request")
		}
		req.ContentLength = length
		resp, requestErr := storage.Do(req)
		if requestErr != nil {
			return result, storageError(ctx, "storage upload request failed")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			responseErr := storageResponseError(ctx, resp)
			resp.Body.Close()
			return result, responseErr
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	after, statErr := input.Stat()
	if statErr != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return result, localError("local file changed during upload; upload was not completed", statErr)
	}
	var complete File
	completionAttempted = true
	if err := c.Request(ctx, "/v2/file/complete", map[string]any{
		"drive_id": folder.DriveID, "file_id": created.FileID, "upload_id": created.UploadID,
	}, &complete); err != nil {
		return result, err
	}
	if complete.FileID != created.FileID {
		return result, upstream("completed upload response did not match the created file")
	}
	if complete.ContentHash != "" {
		result.File.ContentHash = complete.ContentHash
	}
	result.File.CreatedAt = complete.CreatedAt
	result.File.UpdatedAt = complete.UpdatedAt
	finished = true
	return result, nil
}

func destinationPath(dest string, file File, overwrite bool) (string, error) {
	if dest == "" {
		dest = "."
	}
	info, err := os.Stat(dest)
	if err == nil && info.IsDir() {
		if err := validateName(file.Name); err != nil {
			return "", Invalid("remote name is unsafe locally; provide an explicit destination filename")
		}
		dest = filepath.Join(dest, file.Name)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", localError("cannot inspect download destination", err)
	} else if err != nil && strings.HasSuffix(dest, string(os.PathSeparator)) {
		return "", localError("download destination directory does not exist", err)
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", localError("cannot resolve download destination", err)
	}
	if existing, err := os.Lstat(abs); err == nil {
		if !overwrite {
			return "", conflict("local destination already exists; pass --overwrite to replace it")
		}
		if existing.IsDir() {
			return "", Invalid("download destination must be a file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", localError("cannot inspect download destination", err)
	}
	parent, err := os.Stat(filepath.Dir(abs))
	if err != nil || !parent.IsDir() {
		return "", localError("download destination parent must be an existing directory", err)
	}
	return abs, nil
}

type downloadWriter struct {
	io.Writer
	err error
}

func (w *downloadWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (c *Client) Download(ctx context.Context, ref, dest string, overwrite, dry bool) (Transfer, error) {
	file, err := c.Resolve(ctx, ref)
	if err != nil {
		return Transfer{}, err
	}
	if file.Type != "file" {
		return Transfer{}, Invalid("download supports files only; recursive folder download is not supported")
	}
	if file.Size < 0 {
		return Transfer{}, upstream("service returned an invalid file size")
	}
	abs, err := destinationPath(dest, file, overwrite)
	if err != nil {
		return Transfer{}, err
	}
	result := Transfer{Operation: "download", DryRun: dry, File: file, LocalPath: abs, Bytes: file.Size}
	if dry {
		return result, nil
	}
	var signed struct {
		URL string `json:"url"`
	}
	if err := c.Request(ctx, "/v2/file/get_download_url", map[string]any{
		"drive_id": file.DriveID, "file_id": file.FileID, "expire_sec": 900,
	}, &signed); err != nil {
		return result, err
	}
	if err := storageURL(signed.URL); err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.URL, nil)
	if err != nil {
		return result, storageError(ctx, "cannot construct storage download request")
	}
	req.Header.Set("User-Agent", storageUserAgent)
	req.Header.Set("Referer", storageReferer)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := c.storageClient().Do(req)
	if err != nil {
		return result, storageError(ctx, "storage download request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, storageResponseError(ctx, resp)
	}
	temp, err := os.CreateTemp(filepath.Dir(abs), ".aliyundrive-download-*")
	if err != nil {
		return result, localError("cannot create temporary download file", err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	limit := file.Size
	if limit < int64(^uint64(0)>>1) {
		limit++
	}
	writer := &downloadWriter{Writer: temp}
	written, err := io.Copy(writer, io.LimitReader(resp.Body, limit))
	if err != nil {
		if ctx.Err() != nil {
			return result, canceled(ctx.Err())
		}
		if writer.err != nil {
			return result, localError("download stream could not be written completely", err)
		}
		return result, storageError(ctx, "download stream could not be read completely")
	}
	if written != file.Size {
		return result, upstream("downloaded file size did not match remote metadata")
	}
	if err := ctx.Err(); err != nil {
		return result, canceled(err)
	}
	if err := temp.Sync(); err != nil {
		return result, localError("cannot flush temporary download file", err)
	}
	if err := temp.Close(); err != nil {
		return result, localError("cannot close temporary download file", err)
	}
	if err := ctx.Err(); err != nil {
		return result, canceled(err)
	}
	if overwrite {
		if err := os.Rename(temp.Name(), abs); err != nil {
			return result, localError("cannot publish downloaded file", err)
		}
	} else {
		// Hard-link publication fails atomically if another process creates the destination.
		if err := os.Link(temp.Name(), abs); err != nil {
			if errors.Is(err, os.ErrExist) {
				return result, conflict("local destination appeared during download; it was not overwritten")
			}
			return result, localError("cannot publish downloaded file without overwriting", err)
		}
	}
	result.Bytes = written
	return result, nil
}
