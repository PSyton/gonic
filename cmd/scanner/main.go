package main

import (
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhowden/tag"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	"github.com/karrick/godirwalk"

	"github.com/sentriz/gonic/db"
)

var (
	orm *gorm.DB
	tx  *gorm.DB
	// seenTracks is used to keep every track we've seen so that
	// we can later remove old tracks from the database
	seenTracks = make(map[string]bool)
	// seenDirs is used for inserting to the folders table (for browsing
	// by folders instead of tags) which helps us work out a folder's
	// parent folder id
	seenDirs = make(dirStack, 0)
)

func isCover(filename string) bool {
	_, ok := coverFilenames[strings.ToLower(filename)]
	return ok
}

func readTags(fullPath string) (tag.Metadata, error) {
	trackData, err := os.Open(fullPath)
	if err != nil {
		return nil, fmt.Errorf("when tags from disk: %v", err)
	}
	defer trackData.Close()
	tags, err := tag.ReadFrom(trackData)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// handleFolder is for browse by folders, while handleFile is for both
func handleFolder(fullPath string, info *godirwalk.Dirent) error {
	stat, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("when stating folder: %v", err)
	}
	modTime := stat.ModTime()
	folder := db.Folder{
		Path: fullPath,
	}
	// skip if the record exists and hasn't been modified since
	// the last scan
	err = tx.Where(folder).First(&folder).Error
	if !gorm.IsRecordNotFoundError(err) &&
		modTime.Before(folder.UpdatedAt) {
		// even though we don't want to update this record,
		// add it to seenDirs now that we have the id
		seenDirs.Push(folder.ID)
		return nil
	}
	_, folderName := path.Split(fullPath)
	folder.ParentID = seenDirs.Peek()
	folder.Name = folderName
	// save the record with new parent id, then add the new
	// current id to seenDirs
	tx.Save(&folder)
	seenDirs.Push(folder.ID)
	return nil
}

func handleFile(fullPath string, info *godirwalk.Dirent) error {
	stat, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("when stating file: %v", err)
	}
	modTime := stat.ModTime()
	_, filename := path.Split(fullPath)
	if isCover(filename) {
		return nil
	}
	longExt := filepath.Ext(filename)
	extension := strings.ToLower(longExt[1:])
	// check if us audio and save mime type for later
	mime, ok := audioExtensions[extension]
	if !ok {
		return nil
	}
	// add the full path to the seen set. see the comment above
	// seenTracks for more
	seenTracks[fullPath] = true
	// set track basics
	track := db.Track{
		Path: fullPath,
	}
	// skip if the record exists and hasn't been modified since
	// the last scan
	err = tx.Where(track).First(&track).Error
	if !gorm.IsRecordNotFoundError(err) &&
		modTime.Before(track.UpdatedAt) {
		return nil
	}
	tags, err := readTags(fullPath)
	if err != nil {
		return fmt.Errorf("when reading tags: %v", err)
	}
	trackNumber, totalTracks := tags.Track()
	discNumber, totalDiscs := tags.Disc()
	track.Path = fullPath
	track.Title = tags.Title()
	track.Artist = tags.Artist()
	track.DiscNumber = discNumber
	track.TotalDiscs = totalDiscs
	track.TotalTracks = totalTracks
	track.TrackNumber = trackNumber
	track.Year = tags.Year()
	track.Suffix = extension
	track.ContentType = mime
	track.Size = int(stat.Size())
	track.FolderID = seenDirs.Peek()
	// set album artist {
	albumArtist := db.AlbumArtist{
		Name: tags.AlbumArtist(),
	}
	err = tx.Where(albumArtist).First(&albumArtist).Error
	if gorm.IsRecordNotFoundError(err) {
		albumArtist.Name = tags.AlbumArtist()
		tx.Save(&albumArtist)
	}
	track.AlbumArtistID = albumArtist.ID
	// set album
	album := db.Album{
		AlbumArtistID: albumArtist.ID,
		Title:         tags.Album(),
	}
	err = tx.Where(album).First(&album).Error
	if gorm.IsRecordNotFoundError(err) {
		album.Title = tags.Album()
		album.AlbumArtistID = albumArtist.ID
		tx.Save(&album)
	}
	track.AlbumID = album.ID
	// save track
	tx.Save(&track)
	return nil
}

func handleFolderCompletion(fullPath string, info *godirwalk.Dirent) error {
	seenDirs.Pop()
	log.Printf("processed folder `%s`\n", fullPath)
	return nil
}

func handleItem(fullPath string, info *godirwalk.Dirent) error {
	// TODO: stat here instead of in each handler
	if info.IsDir() {
		return handleFolder(fullPath, info)
	}
	return handleFile(fullPath, info)
}

func createDatabase() {
	tx.AutoMigrate(
		&db.Album{},
		&db.AlbumArtist{},
		&db.Track{},
		&db.Cover{},
		&db.User{},
		&db.Setting{},
		&db.Play{},
		&db.Folder{},
	)
	// set starting value for `albums` table's
	// auto increment
	tx.Exec(`
        INSERT INTO sqlite_sequence(name, seq)
        SELECT 'albums', 500000
        WHERE  NOT EXISTS (SELECT *
                           FROM   sqlite_sequence);
	`)
	// create the first user if there is none
	tx.FirstOrCreate(&db.User{}, db.User{
		Name:     "admin",
		Password: "admin",
		IsAdmin:  true,
	})
}

func cleanDatabase() {
	// delete tracks not on filesystem
	var tracks []*db.Track
	tx.Select("id, path").Find(&tracks)
	for _, track := range tracks {
		_, ok := seenTracks[track.Path]
		if ok {
			continue
		}
		tx.Delete(&track)
		log.Println("removed", track.Path)
	}
	// delete albums without tracks
	tx.Exec(`
        DELETE FROM albums
        WHERE  (SELECT count(id)
                FROM   tracks
                WHERE  album_id = albums.id) = 0;
	`)
	// delete artists without tracks
	tx.Exec(`
        DELETE FROM album_artists
        WHERE  (SELECT count(id)
                FROM   albums
                WHERE  album_artist_id = album_artists.id) = 0;
	`)
}

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("usage: %s <path to music>", os.Args[0])
	}
	orm = db.New()
	orm.SetLogger(log.New(os.Stdout, "gorm ", 0))
	tx = orm.Begin()
	createDatabase()
	startTime := time.Now()
	err := godirwalk.Walk(os.Args[1], &godirwalk.Options{
		Callback:             handleItem,
		PostChildrenCallback: handleFolderCompletion,
		Unsorted:             true,
	})
	if err != nil {
		log.Fatalf("error when walking: %v\n", err)
	}
	log.Printf("scanned in %s\n", time.Since(startTime))
	startTime = time.Now()
	cleanDatabase()
	log.Printf("cleaned in %s\n", time.Since(startTime))
	tx.Commit()
}
