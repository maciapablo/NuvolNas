package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB
var dbPath string

type DriveItem struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	FullPath string `json:"full_path"`
}

type FileItem struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"is_dir"`
	ModTime    string `json:"mod_time"`
	IsFavorite bool   `json:"is_favorite"`
	FileType   string `json:"file_type"`
}

func main() {
	if runtime.GOOS == "windows" {
		wd, _ := os.Getwd()
		dbPath = filepath.Join(wd, "nas.db")
	} else {
		dbPath = "./nas.db"
	}

	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Println("Error crítico al abrir base de datos:", err)
		return
	}
	defer db.Close()

	db.Exec(`
		CREATE TABLE IF NOT EXISTS metadata (
			path TEXT PRIMARY KEY,
			is_favorite INTEGER DEFAULT 0
		);
	`)

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	http.HandleFunc("/api/drives/list", listDrivesHandler)
	http.HandleFunc("/api/files/list", listDirectoryHandler)
	http.HandleFunc("/api/files/photos", listPhotosHandler)
	http.HandleFunc("/api/files/upload", uploadHandler)
	http.HandleFunc("/api/files/download", downloadHandler)
	http.HandleFunc("/api/folders/create", createFolderHandler)
	http.HandleFunc("/api/files/favorite", favoriteHandler)
	http.HandleFunc("/api/files/delete", deleteHandler)
	http.HandleFunc("/api/system/storage", storageHandler)

	fmt.Println("Servidor NAS (Versión Definitiva) corriendo en http://localhost:8080")
	http.ListenAndServe(":8080", nil)
}

func obtenerUnidadesConectadas() []DriveItem {
	var drives []DriveItem

	if runtime.GOOS == "windows" {
		for _, letter := range []string{"C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q", "R", "S", "T", "U", "V", "W", "X", "Y", "Z"} {
			rootPath := letter + ":\\"
			if _, err := os.Stat(rootPath); err == nil {
				driveType := "flashdrive"
				name := "USB_" + letter
				if letter == "C" {
					driveType = "ssd"
					name = "Sistema_C"
				} else if letter == "D" || letter == "E" {
					driveType = "ssd"
					name = "SSD_" + letter
				}
				drives = append(drives, DriveItem{
					Name:     name,
					Path:     name,
					Type:     driveType,
					FullPath: rootPath,
				})
			}
		}
	} else {
		mountPoints := []string{"/mnt", "/media"}
		seen := make(map[string]bool)
		for _, mp := range mountPoints {
			entries, err := os.ReadDir(mp)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() {
					name := e.Name()
					fullPath := filepath.Join(mp, name)
					if seen[fullPath] {
						continue
					}
					seen[fullPath] = true

					driveType := "flashdrive"
					lowerName := strings.ToLower(name)
					if strings.Contains(lowerName, "ssd") || strings.Contains(lowerName, "nvme") {
						driveType = "ssd"
					}
					drives = append(drives, DriveItem{
						Name:     name,
						Path:     name,
						Type:     driveType,
						FullPath: fullPath,
					})
				}
			}
		}
	}
	return drives
}

func resolverRutaFisica(subpath string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(subpath))
	if cleanPath == "." || cleanPath == "" || cleanPath == "undefined" || cleanPath == "null" || cleanPath == "unit" {
		return "", fmt.Errorf("ruta raíz")
	}

	parts := strings.Split(cleanPath, string(filepath.Separator))
	driveName := parts[0]

	drives := obtenerUnidadesConectadas()
	var targetDrivePath string
	for _, d := range drives {
		if d.Name == driveName {
			targetDrivePath = d.FullPath
			break
		}
	}

	if targetDrivePath == "" {
		return "", fmt.Errorf("unidad no encontrada")
	}

	subParts := parts[1:]
	fullPath := filepath.Join(targetDrivePath, filepath.Join(subParts...))
	if !strings.HasPrefix(fullPath, targetDrivePath) {
		return "", fmt.Errorf("acceso no autorizado")
	}
	return fullPath, nil
}

func listDrivesHandler(w http.ResponseWriter, r *http.Request) {
	drives := obtenerUnidadesConectadas()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(drives)
}

func listDirectoryHandler(w http.ResponseWriter, r *http.Request) {
	subpath := r.URL.Query().Get("path")

	if subpath == "" || subpath == "." || subpath == "undefined" || subpath == "null" || subpath == "unit" {
		drives := obtenerUnidadesConectadas()
		var items []FileItem
		for _, d := range drives {
			items = append(items, FileItem{
				Name:       d.Name,
				Path:       d.Path,
				Size:       0,
				IsDir:      true,
				ModTime:    "",
				IsFavorite: false,
				FileType:   d.Type,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
		return
	}

	fullPath, err := resolverRutaFisica(subpath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]FileItem{})
		return
	}

	type result struct {
		entries []os.DirEntry
		err     error
	}
	ch := make(chan result, 1)

	go func() {
		entries, err := os.ReadDir(fullPath)
		ch <- result{entries: entries, err: err}
	}()

	var entries []os.DirEntry
	select {
	case res := <-ch:
		if res.err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]FileItem{})
			return
		}
		entries = res.entries
	case <-time.After(3 * time.Second):
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]FileItem{})
		return
	}

	var items []FileItem
	for _, entry := range entries {
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if lowerName == "system volume information" || lowerName == "$recycle.bin" || lowerName == "pagefile.sys" || lowerName == "hiberfil.sys" || lowerName == "swapfile.sys" || name == "nas_trash" || name == ".DS_Store" {
			continue
		}

		isDir := entry.IsDir()
		relPath := filepath.ToSlash(filepath.Join(subpath, name))
		fileType := "other"
		ext := strings.ToLower(filepath.Ext(name))

		var size int64 = 0
		var modTimeStr string = ""

		if isDir {
			fileType = "folder"
		} else {
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".heic" {
				fileType = "photo"
			} else if ext == ".mp4" || ext == ".mkv" || ext == ".mov" || ext == ".avi" || ext == ".webm" {
				fileType = "video"
			} else {
				fileType = "other"
			}
			if info, err := entry.Info(); err == nil {
				size = info.Size()
				modTimeStr = info.ModTime().Format("2006-01-02 15:04:05")
			}
		}

		var isFav int
		if db != nil {
			db.QueryRow("SELECT is_favorite FROM metadata WHERE path = ?", relPath).Scan(&isFav)
		}

		items = append(items, FileItem{
			Name:       name,
			Path:       relPath,
			Size:       size,
			IsDir:      isDir,
			ModTime:    modTimeStr,
			IsFavorite: isFav == 1,
			FileType:   fileType,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// Endpoint para listar fotos y videos de forma global en todas las unidades conectadas
func listPhotosHandler(w http.ResponseWriter, r *http.Request) {
	drives := obtenerUnidadesConectadas()
	var allMedia []FileItem

	for _, d := range drives {
		filepath.Walk(d.FullPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			name := info.Name()
			lower := strings.ToLower(name)
			if info.IsDir() {
				if lower == "windows" || lower == "program files" || lower == "program files (x86)" || lower == "system volume information" || lower == "$recycle.bin" {
					return filepath.SkipDir
				}
				return nil
			}

			ext := strings.ToLower(filepath.Ext(name))
			var fType string
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".heic" {
				fType = "photo"
			} else if ext == ".mp4" || ext == ".mkv" || ext == ".mov" || ext == ".avi" || ext == ".webm" {
				fType = "video"
			} else {
				return nil
			}

			rel, err := filepath.Rel(d.FullPath, path)
			if err != nil {
				return nil
			}
			relPath := filepath.ToSlash(filepath.Join(d.Name, rel))

			var isFav int
			if db != nil {
				db.QueryRow("SELECT is_favorite FROM metadata WHERE path = ?", relPath).Scan(&isFav)
			}

			allMedia = append(allMedia, FileItem{
				Name:       name,
				Path:       relPath,
				Size:       info.Size(),
				IsDir:      false,
				ModTime:    info.ModTime().Format("2006-01-02 15:04:05"),
				IsFavorite: isFav == 1,
				FileType:   fType,
			})
			return nil
		})
	}

	// Ordenar por fecha de modificación descendente (más reciente primero)
	sort.Slice(allMedia, func(i, j int) bool {
		return allMedia[i].ModTime > allMedia[j].ModTime
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allMedia)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("file")
	subpath := r.FormValue("path")

	if err != nil || subpath == "" {
		http.Error(w, "Selecciona una unidad o carpeta de destino válida", http.StatusBadRequest)
		return
	}
	defer file.Close()

	targetDir, err := resolverRutaFisica(subpath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	os.MkdirAll(targetDir, 0755)

	dstPath := filepath.Join(targetDir, header.Filename)
	dst, err := os.Create(dstPath)
	if err != nil {
		http.Error(w, "Error al crear archivo", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	io.Copy(dst, file)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func createFolderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	name := r.FormValue("name")
	subpath := r.FormValue("path")

	if subpath == "" {
		http.Error(w, "No se puede crear carpetas en la raíz principal", http.StatusBadRequest)
		return
	}

	targetDir, err := resolverRutaFisica(filepath.Join(subpath, name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	os.MkdirAll(targetDir, 0755)
	w.WriteHeader(http.StatusOK)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	subpath := r.URL.Query().Get("path")
	fullPath, err := resolverRutaFisica(subpath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, fullPath)
}

func favoriteHandler(w http.ResponseWriter, r *http.Request) {
	subpath := r.URL.Query().Get("path")
	var currentFav int
	if db != nil {
		db.QueryRow("SELECT is_favorite FROM metadata WHERE path = ?", subpath).Scan(&currentFav)
	}

	newFav := 1
	if currentFav == 1 {
		newFav = 0
	}

	if db != nil {
		db.Exec("INSERT INTO metadata (path, is_favorite) VALUES (?, ?) ON CONFLICT(path) DO UPDATE SET is_favorite = ?", subpath, newFav, newFav)
	}
	w.WriteHeader(http.StatusOK)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	subpath := r.URL.Query().Get("path")
	fullPath, err := resolverRutaFisica(subpath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	os.RemoveAll(fullPath)
	if db != nil {
		db.Exec("DELETE FROM metadata WHERE path = ?", subpath)
	}
	w.WriteHeader(http.StatusOK)
}

func storageHandler(w http.ResponseWriter, r *http.Request) {
	var totalSize int64
	drives := obtenerUnidadesConectadas()

	for _, d := range drives {
		filepath.Walk(d.FullPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return filepath.SkipDir
			}
			name := info.Name()
			lower := strings.ToLower(name)
			if info.IsDir() {
				if lower == "windows" || lower == "program files" || lower == "program files (x86)" || lower == "system volume information" || lower == "$recycle.bin" {
					return filepath.SkipDir
				}
			} else {
				totalSize += info.Size()
			}
			return nil
		})
	}

	var totalCapacity int64 = 2000 * 1024 * 1024 * 1024
	percentage := 0
	if totalCapacity > 0 {
		percentage = int((totalSize * 100) / totalCapacity)
	}

	info := map[string]interface{}{
		"used_bytes":      totalSize,
		"total_bytes":     totalCapacity,
		"used_formatted":  formatBytes(totalSize),
		"total_formatted": "2 TB",
		"percentage":      percentage,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sB", float64(bytes)/float64(div), "KMGTPE"[exp:exp+1])
}
