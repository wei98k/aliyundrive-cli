package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return Invalid("name must be a nonempty filename without slashes or dot traversal")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return Invalid("name must not contain control characters")
		}
	}
	return nil
}

func remoteParts(ref string) ([]string, error) {
	if !strings.HasPrefix(ref, "/") {
		return nil, Invalid("remote path must be absolute; use /path, root, or id:FILE_ID")
	}
	if ref == "/" {
		return nil, nil
	}
	parts := strings.Split(strings.TrimSuffix(ref[1:], "/"), "/")
	for _, part := range parts {
		if err := validateName(part); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

func validID(id string) bool {
	if id == "" || id == "." || id == ".." || !utf8.ValidString(id) || strings.ContainsAny(id, "/\\") {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func notFound(message string) error {
	return E("NOT_FOUND", message, "Check the reference with ls or search.", 4)
}

func conflict(message string) error {
	return E("CONFLICT", message, "Use an unambiguous id:FILE_ID reference or choose another name.", 5)
}

func upstream(message string) error {
	return E("UPSTREAM_ERROR", message, "Retry or check the service status.", 6)
}

func (c *Client) File(ctx context.Context, id string) (File, error) {
	if !validID(id) {
		return File{}, Invalid("invalid file ID")
	}
	driveID, err := c.EnsureDrive(ctx)
	if err != nil {
		return File{}, err
	}
	if id == "root" {
		return File{FileID: "root", DriveID: driveID, Name: "/", Type: "folder"}, nil
	}
	var response struct {
		File
		Trashed bool `json:"trashed"`
	}
	if err := c.Request(ctx, "/v2/file/get", map[string]any{"drive_id": driveID, "file_id": id}, &response); err != nil {
		return File{}, err
	}
	if response.Trashed {
		return File{}, notFound("file is in the recycle bin")
	}
	if response.FileID != id {
		return File{}, upstream("file metadata did not match the requested ID")
	}
	if response.DriveID == "" {
		response.DriveID = driveID
	} else if response.DriveID != driveID {
		return File{}, upstream("file metadata did not match the selected drive")
	}
	return response.File, nil
}

func (c *Client) Resolve(ctx context.Context, ref string) (File, error) {
	if ref == "root" || ref == "/" {
		return c.File(ctx, "root")
	}
	if strings.HasPrefix(ref, "id:") {
		return c.File(ctx, strings.TrimPrefix(ref, "id:"))
	}
	parts, err := remoteParts(ref)
	if err != nil {
		return File{}, err
	}
	current, err := c.File(ctx, "root")
	if err != nil {
		return File{}, err
	}
	for _, name := range parts {
		if current.Type != "folder" {
			return File{}, Invalid("an intermediate remote path component is not a folder")
		}
		current, err = c.findChild(ctx, current.FileID, name)
		if err != nil {
			return File{}, err
		}
	}
	return current, nil
}

func (c *Client) findChild(ctx context.Context, parent, name string) (File, error) {
	page, err := c.listID(ctx, parent, 100, "", true)
	if err != nil {
		return File{}, err
	}
	var found *File
	for _, item := range page.Items {
		if item.Name == name {
			if found != nil && found.FileID != item.FileID {
				return File{}, conflict(fmt.Sprintf("multiple files have the exact name %q", name))
			}
			copy := item
			found = &copy
		}
	}
	if found == nil {
		return File{}, notFound(fmt.Sprintf("remote entry %q does not exist", name))
	}
	return *found, nil
}

func validateLimit(limit int) error {
	if limit < 1 || limit > 100 {
		return Invalid("limit must be between 1 and 100")
	}
	return nil
}

func validateMarker(marker string) error {
	if len(marker) > 16384 || !utf8.ValidString(marker) {
		return Invalid("pagination marker is invalid or too long")
	}
	for _, r := range marker {
		if unicode.IsControl(r) {
			return Invalid("pagination marker must not contain control characters")
		}
	}
	return nil
}

func collectPages(ctx context.Context, marker string, all bool, fetch func(string) (Page, error)) (Page, error) {
	result := Page{Items: []File{}}
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return Page{}, canceled(err)
		}
		if seen[marker] {
			return Page{}, upstream("service returned a repeated pagination marker")
		}
		seen[marker] = true
		page, err := fetch(marker)
		if err != nil {
			return Page{}, err
		}
		result.Items = append(result.Items, page.Items...)
		result.NextMarker = page.NextMarker
		if !all || page.NextMarker == "" {
			return result, nil
		}
		marker = page.NextMarker
	}
}

func (c *Client) listID(ctx context.Context, parent string, limit int, marker string, all bool) (Page, error) {
	driveID, err := c.EnsureDrive(ctx)
	if err != nil {
		return Page{}, err
	}
	return collectPages(ctx, marker, all, func(next string) (Page, error) {
		var page Page
		err := c.Request(ctx, "/adrive/v3/file/list", map[string]any{
			"drive_id": driveID, "parent_file_id": parent, "limit": limit, "marker": next,
			"order_by": "name", "order_direction": "ASC", "fields": "*",
		}, &page)
		if err == nil {
			for i := range page.Items {
				if !validID(page.Items[i].FileID) || page.Items[i].FileID == "root" {
					return Page{}, upstream("service returned an invalid child file ID")
				}
				page.Items[i].DriveID = driveID
				page.Items[i].ParentFileID = parent
			}
		}
		return page, err
	})
}

func (c *Client) List(ctx context.Context, parent string, limit int, marker string, all bool) (Page, error) {
	if err := validateLimit(limit); err != nil {
		return Page{}, err
	}
	if err := validateMarker(marker); err != nil {
		return Page{}, err
	}
	folder, err := c.Resolve(ctx, parent)
	if err != nil {
		return Page{}, err
	}
	if folder.Type != "folder" {
		return Page{}, Invalid("list target must be a folder")
	}
	return c.listID(ctx, folder.FileID, limit, marker, all)
}

func (c *Client) Search(ctx context.Context, name string, limit int, marker string, all bool) (Page, error) {
	if err := validateLimit(limit); err != nil {
		return Page{}, err
	}
	if err := validateMarker(marker); err != nil {
		return Page{}, err
	}
	if name == "" || !utf8.ValidString(name) {
		return Page{}, Invalid("search name must be nonempty valid UTF-8")
	}
	driveID, err := c.EnsureDrive(ctx)
	if err != nil {
		return Page{}, err
	}
	quoted, _ := json.Marshal(name)
	return collectPages(ctx, marker, all, func(next string) (Page, error) {
		var page Page
		err := c.Request(ctx, "/adrive/v3/file/search", map[string]any{
			"drive_id": driveID, "query": "name match " + string(quoted), "limit": limit,
			"marker": next, "order_by": "name ASC",
		}, &page)
		return page, err
	})
}

func (c *Client) Mkdir(ctx context.Context, remote string, parents, dry bool) (Mutation, error) {
	parts, err := remoteParts(remote)
	if err != nil {
		return Mutation{}, err
	}
	if len(parts) == 0 {
		return Mutation{}, Invalid("the drive root cannot be created or modified")
	}
	current, err := c.File(ctx, "root")
	if err != nil {
		return Mutation{}, err
	}
	result := Mutation{Operation: "mkdir", DryRun: dry, Destination: remote, Created: []File{}}
	planned := false
	for i, name := range parts {
		var child File
		if !planned {
			child, err = c.findChild(ctx, current.FileID, name)
			if err == nil {
				if child.Type != "folder" {
					return Mutation{}, conflict("a non-folder already occupies the requested path")
				}
				if i == len(parts)-1 && !parents {
					return Mutation{}, conflict("folder already exists")
				}
				current = child
				continue
			}
			if e, ok := err.(*Error); !ok || e.ExitCode != 4 {
				return Mutation{}, err
			}
		}
		if !parents && i < len(parts)-1 {
			return Mutation{}, notFound("parent folder does not exist; use --parents to create it")
		}
		child = File{DriveID: current.DriveID, ParentFileID: current.FileID, Name: name, Type: "folder"}
		if dry {
			planned = true
		} else {
			var response struct {
				File
				FileName string `json:"file_name"`
			}
			err = c.Request(ctx, "/adrive/v2/file/createWithFolders", map[string]any{
				"drive_id": current.DriveID, "parent_file_id": current.FileID, "name": name,
				"type": "folder", "check_name_mode": "refuse",
			}, &response)
			if err != nil {
				return result, err
			}
			if response.FileID == "" {
				return result, upstream("created folder response did not include an ID")
			}
			child.FileID = response.FileID
		}
		result.Created = append(result.Created, child)
		current = child
	}
	result.File = current
	return result, nil
}

func mutable(file File) error {
	if file.FileID == "root" {
		return Invalid("the drive root cannot be modified")
	}
	if file.ParentFileID == "" {
		return upstream("file metadata did not include a parent folder ID")
	}
	return nil
}

func rejectRoot(ref string) error {
	if ref == "/" || ref == "root" || ref == "id:root" {
		return Invalid("the drive root cannot be modified")
	}
	return nil
}

func (c *Client) checkNameFree(ctx context.Context, parent, name, exceptID string) error {
	page, err := c.listID(ctx, parent, 100, "", true)
	if err != nil {
		return err
	}
	for _, item := range page.Items {
		if item.Name == name && item.FileID != exceptID {
			return conflict("a file or folder with that name already exists")
		}
	}
	return nil
}

func (c *Client) Rename(ctx context.Context, ref, name string, dry bool) (Mutation, error) {
	if err := validateName(name); err != nil {
		return Mutation{}, err
	}
	if err := rejectRoot(ref); err != nil {
		return Mutation{}, err
	}
	file, err := c.Resolve(ctx, ref)
	if err != nil {
		return Mutation{}, err
	}
	if err := mutable(file); err != nil {
		return Mutation{}, err
	}
	if err := c.checkNameFree(ctx, file.ParentFileID, name, file.FileID); err != nil {
		return Mutation{}, err
	}
	if !dry && file.Name != name {
		if err := c.Request(ctx, "/v3/file/update", map[string]any{
			"drive_id": file.DriveID, "file_id": file.FileID, "name": name, "check_name_mode": "refuse",
		}, nil); err != nil {
			return Mutation{}, err
		}
	}
	file.Name = name
	return Mutation{Operation: "rename", DryRun: dry, File: file, Destination: name}, nil
}

func (c *Client) Move(ctx context.Context, ref, parent string, dry bool) (Mutation, error) {
	if err := rejectRoot(ref); err != nil {
		return Mutation{}, err
	}
	file, err := c.Resolve(ctx, ref)
	if err != nil {
		return Mutation{}, err
	}
	if err := mutable(file); err != nil {
		return Mutation{}, err
	}
	folder, err := c.Resolve(ctx, parent)
	if err != nil {
		return Mutation{}, err
	}
	if folder.Type != "folder" {
		return Mutation{}, Invalid("move destination must be a folder")
	}
	if file.Type == "folder" {
		ancestor := folder
		seen := map[string]bool{}
		for {
			if ancestor.FileID == file.FileID {
				return Mutation{}, Invalid("a folder cannot be moved into itself or its descendants")
			}
			if ancestor.FileID == "root" {
				break
			}
			if seen[ancestor.FileID] || ancestor.ParentFileID == "" {
				return Mutation{}, upstream("invalid folder ancestry returned by the service")
			}
			seen[ancestor.FileID] = true
			ancestor, err = c.File(ctx, ancestor.ParentFileID)
			if err != nil {
				return Mutation{}, err
			}
		}
	}
	if err := c.checkNameFree(ctx, folder.FileID, file.Name, file.FileID); err != nil {
		return Mutation{}, err
	}
	if !dry && file.ParentFileID != folder.FileID {
		if err := c.Request(ctx, "/v3/file/move", map[string]any{
			"drive_id": file.DriveID, "file_id": file.FileID, "to_drive_id": folder.DriveID,
			"to_parent_file_id": folder.FileID, "check_name_mode": "refuse", "auto_rename": false,
		}, nil); err != nil {
			return Mutation{}, err
		}
	}
	file.ParentFileID = folder.FileID
	return Mutation{Operation: "move", DryRun: dry, File: file, Destination: folder.FileID}, nil
}

func (c *Client) Trash(ctx context.Context, ref string, dry bool) (Mutation, error) {
	if err := rejectRoot(ref); err != nil {
		return Mutation{}, err
	}
	file, err := c.Resolve(ctx, ref)
	if err != nil {
		return Mutation{}, err
	}
	if err := mutable(file); err != nil {
		return Mutation{}, err
	}
	result := Mutation{Operation: "trash", DryRun: dry, File: file}
	if !dry {
		var response struct {
			AsyncTaskID string `json:"async_task_id"`
		}
		if err := c.Request(ctx, "/v2/recyclebin/trash", map[string]any{"drive_id": file.DriveID, "file_id": file.FileID}, &response); err != nil {
			return Mutation{}, err
		}
		result.AsyncTaskID = response.AsyncTaskID
	}
	return result, nil
}
