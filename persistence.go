package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"log"
	"os"
	"strings"
)

// DESIGN#1
// Search is just by tags with a rather badly constructed search query builder
// Initial search is fine, however there should be some further search refinement options

// DESIGN#2
// I still haven't done paginated results wahey

func initDB(db *sql.DB) error {
	log.Println("Initialising database")

	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return err
	}
	defer tx.Rollback()

	tables := []string{
		`create table if not exists tags (
			filename text,
			name text,
			val text,
			primary key (filename, name)
		);`,

		`create table if not exists thumbnails (
			filename text,
			image blob,
			primary key (filename)
		);`,

		`create table if not exists checked (
			filename text,
			primary key (filename)
		);`,
	}

	for _, table := range tables {
		_, err = tx.Exec(table)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	// Only really necessary when checked table was introduced
	// Now everything gets added to checked correctly already
	// So this is redundant -- but no harm in keeping it around
	_, err = tx.Exec(
		`insert or ignore into checked (filename) 
			select filename 
			from tags 
			where name is 'diskfilename';
		`)
	if err != nil {
		log.Println(err)
		return err
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return err
	}

	err = initTemplates(db)
	if err != nil {
		log.Println(err)
		return err
	}

	err = initRest(db)
	if err != nil {
		log.Println(err)
		return err
	}

	log.Println("Database initialised")

	return nil
}

// "init" template should just be a json array of SQL statements to run without arguments
// intended for creating tables mostly, but why limit yourself
// for example:
// [ "create table if not exists favourites (filename text primary key);" ]
func initRest(db *sql.DB) error {
	raw, err := getTemplate(db, "init")
	if err != nil {
		log.Println(err)
		return err
	}

	var statements []string
	err = json.Unmarshal([]byte(raw), &statements)
	if err != nil {
		log.Println(err)
		return err
	}

	for _, statement := range statements {
		log.Println(statement)
		_, err = db.Exec(statement)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	return nil
}

func addFileToDB(db *sql.DB, filename string) (bool, error) {
	// This is a bit of handwave
	// it's possibly the file is earnestly a media file but an error occurs anyway
	// in that case then it won't be added to the db ever
	// because it is already considered to be checked
	// can fix this easily... think about it
	_, err := db.Exec("insert into checked (filename) values (?);", filename)
	if err != nil {
		return false, nil
	}

	// Assume any failed thumbnail gen means it wasn't a media file
	// what about audio? audio is skipped
	thumbnail, err := CreateThumbnail(filename)
	if err != nil {
		return false, nil
	}

	metadata, err := GetMetadata(filename)
	if err != nil {
		log.Println(err)
		return false, err
	}

	info, err := os.Stat(filename)
	if err != nil {
		log.Println(err)
		return false, err
	}

	metadata["diskfiletime"] = info.ModTime().UTC().Format("2006-01-02T15:04:05")
	metadata["diskfilename"] = filename
	thumbname := fmt.Sprintf("%s.webp", filename)
	metadata["thumbname"] = thumbname

	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return false, err
	}
	defer tx.Rollback()

	stmtThumb, err := tx.Prepare(`
		insert or replace into 
			thumbnails (filename, image) 
			    values (       ?,     ?);
		`)
	if err != nil {
		log.Println(err)
		return false, err
	}
	defer stmtThumb.Close()

	stmtMetadata, err := tx.Prepare(`
		insert or replace into 
			  tags (filename, name, val) 
			values (       ?,    ?,   ?);
		`)
	if err != nil {
		log.Println(err)
		return false, err
	}
	defer stmtMetadata.Close()

	_, err = stmtThumb.Exec(thumbname, thumbnail)
	if err != nil {
		log.Println(err)
		return false, err
	}

	for k, v := range metadata {
		_, err = stmtMetadata.Exec(filename, k, v)
		if err != nil {
			log.Println(err)
			return false, err
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return false, err
	}

	return true, nil
}

func addFilesToDB(db *sql.DB, path string) (int, error) {
	count := 0

	log.Printf("Adding files in %s to db\n", path)

	filenames, err := recls(path)
	if err != nil {
		return count, err
	}
	log.Printf("%v files found\n", len(filenames))

	filenames, err = filesNotInDB(db, filenames)
	if err != nil {
		return count, err
	}
	log.Printf("%v files have not been checked previously\n", len(filenames))

	for _, filename := range filenames {
		ok, err := addFileToDB(db, filename)
		if err != nil {
			return count, err
		}
		if ok {
			count++
		}

	}

	return count, nil
}

func initTemplates(db *sql.DB) error {
	_, err := db.Exec(`
		create table if not exists templates (
			previous integer,
			name text,
			raw text,
			primary key (previous, name)
		);`)
	if err != nil {
		log.Println(err)
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return err
	}
	defer tx.Rollback()

	stmtUpdate, err := tx.Prepare(`
		insert or ignore into 
		templates (name, raw, previous) 
		values    (   ?,   ?,        0);`)
	if err != nil {
		log.Println(err)
		return err
	}
	defer stmtUpdate.Close()

	for name, raw := range starterTemplates {
		_, err = stmtUpdate.Exec(name, raw)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return err
	}

	return nil
}

func getTemplate(db *sql.DB, name string) (string, error) {
	row := db.QueryRow(`
		select raw 
		from templates 
		where name is :name and not rowid in (
			select previous
			from templates
			where name is :name 
		);`, sql.Named("name", name))
	var raw string
	err := row.Scan(&raw)
	if err != nil {
		log.Println(err)
		return raw, err
	}

	return raw, nil
}

func cullMissing(db *sql.DB, dir string) error {
	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("delete from tags where filename is ?;")
	if err != nil {
		log.Println(err)
		return err
	}
	defer stmt.Close()

	count := 0
	rows, err := tx.Query(`
		select filename, val 
		from tags 
		where name is 'diskfilename';`)
	if err != nil {
		log.Println(err)
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var filename string
		var path string
		err = rows.Scan(&filename, &path)
		if err != nil {
			log.Println(err)
			return err
		}

		_, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			// file moved, or removed
			log.Printf("Removing %s\n", path)
			_, err = stmt.Exec(filename)
			if err != nil {
				log.Println(err)
				return err
			}
			count += 1
			continue
		}

		if err != nil {
			log.Println(err)
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return err
	}

	log.Printf("%d missing media items removed\n", count)
	return nil
}

// returns a slice of files which are not in the database
func filesNotInDB(db *sql.DB, filenames []string) ([]string, error) {
	newFiles := make([]string, 0, len(filenames))

	rows, err := db.Query("select filename from checked;")
	if err != nil {
		log.Println(err)
		return newFiles, err
	}
	defer rows.Close()

	dbnames := make(map[string]bool)
	for rows.Next() {
		var filename string
		err = rows.Scan(&filename)
		if err != nil {
			log.Println(err)
			return newFiles, err
		}

		dbnames[filename] = true
	}

	for _, filename := range filenames {
		if _, ok := dbnames[filename]; !ok {
			newFiles = append(newFiles, filename)
		}
	}

	return newFiles, nil
}

type SearchParameters struct {
	Vals        []string
	KeyVals     map[string]string
	Limit       int
	RandomOrder bool
}

// This is unfortunately a SQL query string building function
// sqlite3 FSTS didn't really fit with my table design and goals
// FSTS could be used... but would need to create a specific FSTS table with data modified to suit it
func lookup2(db *sql.DB, params SearchParameters) ([][]string, error) {
	// the final query is made of bricks glued together
	// it mostly builds up a lot of subqueries
	// sqlite3 in-built function "instr" is used a lot
	bricks := make([]string, 0, 20)
	bricks = append(bricks, "select filename, val from tags where name is 'thumbname'")
	glue := "and"
	fills := make([]interface{}, 0, 20)

	for _, v := range params.Vals {
		bricks = append(bricks, glue)
		bricks = append(bricks, `
			filename in (
				select distinct(filename) 
				from tags 
				where instr(lower(val), lower(?)) > 0
			)`)
		fills = append(fills, v)
	}

	for k, v := range params.KeyVals {
		bricks = append(bricks, glue)
		bricks = append(bricks, `
			filename in (
				select filename 
				from tags 
				where name is ? and instr(lower(val), lower(?)) > 0
			)`)
		fills = append(fills, k, v)
	}

	if params.RandomOrder {
		bricks = append(bricks, "order by random()")
	}

	if params.Limit > 0 {
		bricks = append(bricks, "limit ?")
		fills = append(fills, params.Limit)
	}

	bricks = append(bricks, ";")

	rescap := params.Limit
	if rescap == 0 {
		rescap = 50
	}
	res := make([][]string, 0, rescap)

	query := strings.Join(bricks, " ")
	log.Printf("\n%s\n%v\n", query, fills)
	rows, err := db.Query(query, fills...)
	if err != nil {
		return res, err
	}

	for rows.Next() {
		var filename string
		var thumbname string
		err = rows.Scan(&filename, &thumbname)
		if err != nil {
			return res, err
		}
		res = append(res, []string{filename, thumbname})
	}

	return res, err
}

// changes all tags to lowercase
func fixtags(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return err
	}
	defer tx.Rollback()

	upd, err := tx.Prepare("update tags set name = ? where name is ?;")
	if err != nil {
		log.Println(err)
		return err
	}
	defer upd.Close()

	rows, err := tx.Query("select distinct(name) from tags;")
	if err != nil {
		log.Println(err)
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		err = rows.Scan(&name)
		if err != nil {
			log.Println(err)
			return err
		}

		lower := strings.ToLower(name)
		if lower == name {
			continue
		}

		_, err = upd.Exec(lower, name)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return err
	}

	return nil
}

var punctuation = " \r\n\t\"`~()[]{}<>&^%$#@?!+-=_,.:;|/\\*"

func stringsplat(s, cutset string) []string {
	res := make([]string, 0, 100)

	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(cutset, c) {
			if b.Len() > 0 {
				res = append(res, b.String())
				b.Reset()
			}
		} else {
			b.WriteRune(c)
		}
	}

	if b.Len() > 0 {
		res = append(res, b.String())
		b.Reset()
	}

	return res
}

func wordassocs(db *sql.DB) error {
	_, err := db.Exec(`
		create table if not exists wordassocs (
			filename text,
			word text,
			primary key (filename, word) on conflict ignore
		);`)
	if err != nil {
		log.Println(err)
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		log.Println(err)
		return err
	}
	defer tx.Rollback()

	rows, err := db.Query(`
		select filename, val 
		from tags 
		where filename not in (
			select distinct(filename) 
			from wordassocs
		);`)
	if err != nil {
		log.Println(err)
		return err
	}
	defer rows.Close()

	stmtUpdate, err := tx.Prepare(`
		insert into 
			wordassocs (filename, word) 
			values     (       ?,    ?);
		`)
	if err != nil {
		log.Println(err)
		return err
	}
	defer stmtUpdate.Close()

	for rows.Next() {
		var filename string
		var words string
		err = rows.Scan(&filename, &words)
		if err != nil {
			log.Println(err)
			return err
		}

		for _, word := range stringsplat(words, punctuation) {
			word = strings.ToLower(word)

			_, err = stmtUpdate.Exec(filename, word)
			if err != nil {
				log.Println(err)
				return err
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Println(err)
		return err
	}

	return err
}

func parseSearchTerms(formterms []string) SearchParameters {
	terms := make([]string, 0, 50)
	for _, term := range formterms {
		lo := 0
		i := 0

		basegrow := func(x int) {
			if i > lo {
				terms = append(terms, term[lo:i+x])
			}
			lo = i + 1
		}

		grow := func() {
			basegrow(0)
		}

		altgrow := func() {
			basegrow(1)
		}

		type roller func(rune) roller
		var quoteroller, baseroller roller

		baseroller = func(c rune) roller {
			switch c {
			case '"':
				grow()
				return quoteroller

			case ' ':
				grow()
				return baseroller

			case ':':
				altgrow()
				return baseroller

			default:
				return baseroller
			}
		}

		quoteroller = func(c rune) roller {
			switch c {
			case '"':
				grow()
				return baseroller

			default:
				return quoteroller
			}
		}

		// using a state machine, the work loop looks so clean now
		// but how obvious is the state machine code?
		step := baseroller
		for _, c := range term {
			step = step(c)
			i++
		}
		grow()
	}

	log.Printf("Terms:\n")
	for _, term := range terms {
		log.Printf("\t%s\n", term)
	}

	params := SearchParameters{
		Vals:        make([]string, 0, 50),
		KeyVals:     make(map[string]string),
		RandomOrder: true,
		Limit:       50,
	}

	skip := false
	for i, term := range terms {
		if skip {
			skip = false
			continue
		}

		if strings.Contains(term, ":") {
			if len(terms) > i+1 {
				params.KeyVals[term[:len(term)-1]] = terms[i+1]
				skip = true
			}
		} else {
			params.Vals = append(params.Vals, term)
		}
	}

	return params
}
