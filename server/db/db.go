package db

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evcc-io/evcc/util"
	"github.com/glebarez/sqlite"
	"github.com/mitchellh/go-homedir"
	"gorm.io/gorm"
)

var Instance *gorm.DB

func New(driver, dsn string) (*gorm.DB, error) {
	var dialect gorm.Dialector
	var newDb bool
	log := util.NewLogger("db")

	switch driver {
	case "sqlite":
		file, err := homedir.Expand(dsn)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			// db file does not exist, create
			log.INFO.Println("database does not exist, creating...")
			if err := os.MkdirAll(filepath.Dir(file), os.ModePerm); err != nil {
				return nil, err
			}
			newDb = true
		}
		log.INFO.Println("using sqlite database:", file)

		// avoid busy errors
		dialect = sqlite.Open(file + "?_pragma=busy_timeout(5000)")
	// case "postgres":
	// 	dialect = postgres.Open(dsn)
	// case "mysql":
	// 	dialect = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("invalid database type: %s not in [sqlite]", driver)
	}

	db, err := gorm.Open(dialect, &gorm.Config{
		Logger: &Logger{log},
	})
	if err == nil && newDb {
		err = &NewDbError{Message: "new db"}
	}

	return db, err
}

func NewInstance(driver, dsn string) (err error) {
	Instance, err = New(strings.ToLower(driver), dsn)
	return
}

type NewDbError struct {
	Message string
}

func (e *NewDbError) Error() string {
	return e.Message
}
