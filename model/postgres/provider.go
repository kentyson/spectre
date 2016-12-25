package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/DHowett/ghostbin/lib/crypto"
	"github.com/DHowett/ghostbin/model"
	"github.com/Sirupsen/logrus"

	"github.com/GeertJohan/go.rice"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
)

type dbBroker struct {
	*gorm.DB
	sqlDb             *sql.DB
	Logger            logrus.FieldLogger
	ChallengeProvider crypto.ChallengeProvider
}

// User
func (broker *dbBroker) getUserWithQuery(query string, args ...interface{}) (model.User, error) {
	var u dbUser
	if err := broker.Where(query, args...).First(&u).Error; err != nil {
		return nil, err
	}
	u.broker = broker
	return &u, nil
}

func (broker *dbBroker) GetUserNamed(name string) (model.User, error) {
	u, err := broker.getUserWithQuery("name = ?", name)
	return u, err
}

func (broker *dbBroker) GetUserByID(id uint) (model.User, error) {
	return broker.getUserWithQuery("id = ?", id)
}

func (broker *dbBroker) CreateUser(name string) (model.User, error) {
	u := &dbUser{
		Name:   name,
		broker: broker,
	}
	if err := broker.Create(u).Error; err != nil {
		return nil, err
	}
	return u, nil
}

// Paste
func (broker *dbBroker) GenerateNewPasteID(encrypted bool) model.PasteID {
	nbytes, idlen := 4, 5
	if encrypted {
		nbytes, idlen = 5, 8
	}

	for {
		s, _ := generateRandomBase32String(nbytes, idlen)
		return model.PasteIDFromString(s)
	}
}

func (broker *dbBroker) CreatePaste() (model.Paste, error) {
	paste := dbPaste{broker: broker}
	for {
		if err := broker.Create(&paste).Error; err != nil {
			panic(err)
		}
		paste.broker = broker
		return &paste, nil
	}
}

func (broker *dbBroker) CreateEncryptedPaste(method model.PasteEncryptionMethod, passphraseMaterial []byte) (model.Paste, error) {
	if passphraseMaterial == nil {
		return nil, errors.New("model: unacceptable encryption material")
	}
	paste := dbPaste{broker: broker}
	paste.EncryptionSalt, _ = generateRandomBytes(16)
	paste.EncryptionMethod = model.PasteEncryptionMethodAES_CTR
	key, err := model.GetPasteEncryptionCodec(method).DeriveKey(passphraseMaterial, paste.EncryptionSalt)
	if err != nil {
		return nil, err
	}
	paste.encryptionKey = key

	for {
		if err := broker.Create(&paste).Error; err != nil {
			panic(err)
		}
		paste.broker = broker
		return &paste, nil
	}
}

func (broker *dbBroker) GetPaste(id model.PasteID, passphraseMaterial []byte) (model.Paste, error) {
	var paste dbPaste
	if err := broker.Find(&paste, "id = ?", id.String()).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, model.ErrNotFound
		}

		return nil, err
	}
	paste.broker = broker

	// This paste is encrypted
	if paste.IsEncrypted() {
		// If they haven't requested decryption, we can
		// still tell them that a paste exists.
		// It will be a stub/placeholder that only has an ID.
		if passphraseMaterial == nil {
			return &encryptedPastePlaceholder{
				ID: id,
			}, model.ErrPasteEncrypted
		}

		key, err := model.GetPasteEncryptionCodec(paste.EncryptionMethod).DeriveKey(passphraseMaterial, paste.EncryptionSalt)
		if err != nil {
			return nil, model.ErrPasteEncrypted
		}

		ok := model.GetPasteEncryptionCodec(paste.EncryptionMethod).Authenticate(id, paste.EncryptionSalt, key, paste.HMAC)
		if !ok {
			return nil, model.ErrInvalidKey
		}

		paste.encryptionKey = key
	}

	return &paste, nil
}

func (broker *dbBroker) GetPastes(ids []model.PasteID) ([]model.Paste, error) {
	stringIDs := make([]string, len(ids))
	for i, v := range ids {
		stringIDs[i] = string(v)
	}

	var ps []*dbPaste
	if err := broker.Find(&ps, "id in (?)", stringIDs).Error; err != nil {
		return nil, err
	}

	iPastes := make([]model.Paste, len(ps))
	for i, p := range ps {
		p.broker = broker
		if p.IsEncrypted() {
			iPastes[i] = &encryptedPastePlaceholder{
				ID: p.GetID(),
			}
		} else {
			iPastes[i] = p
		}
	}
	return iPastes, nil
}

func (broker *dbBroker) GetExpiringPastes() ([]model.ExpiringPaste, error) {
	var ps []*dbPaste
	if err := broker.Not("expire_at", nil).Select("id, expire_at").Find(&ps).Error; err != nil {
		return nil, err
	}

	eps := make([]model.ExpiringPaste, len(ps))
	for i, p := range ps {
		eps[i] = model.ExpiringPaste{
			PasteID: model.PasteID(p.ID),
			Time:    *p.ExpireAt,
		}
	}
	return eps, nil
}

func (broker *dbBroker) DestroyPaste(id model.PasteID) error {
	tx := broker.Begin()
	if err := tx.Delete(&dbPaste{ID: id.String()}).Error; err != nil && err != gorm.ErrRecordNotFound {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

func (broker *dbBroker) CreateGrant(paste model.Paste) (model.Grant, error) {
	grant := dbGrant{PasteID: paste.GetID().String(), broker: broker}
	for {
		if err := broker.Create(&grant).Error; err != nil {
			panic(err)
		}
		grant.broker = broker
		return &grant, nil
	}
}

func (broker *dbBroker) GetGrant(id model.GrantID) (model.Grant, error) {
	var grant dbGrant
	if err := broker.Find(&grant, "id = ?", string(id)).Error; err != nil {
		return nil, err
	}
	grant.broker = broker
	return &grant, nil
}

func (broker *dbBroker) ReportPaste(p model.Paste) error {
	pID := p.GetID()
	result, err := broker.sqlDb.Exec("UPDATE paste_reports SET count = count + 1 WHERE paste_id = ?", pID.String())
	if nrows, _ := result.RowsAffected(); nrows == 0 {
		_, err = broker.sqlDb.Exec("INSERT INTO paste_reports(paste_id, count) VALUES(?, 1)", pID.String())
		return err
	}

	return err
}

func (broker *dbBroker) GetReport(pID model.PasteID) (model.Report, error) {
	row := broker.sqlDb.QueryRow("SELECT count FROM paste_reports WHERE paste_id = ?", pID.String())

	var count int
	err := row.Scan(&count)
	if err == sql.ErrNoRows {
		return nil, model.ErrNotFound
	} else if err != nil {
		// TODO(DH) errors?
		return nil, err
	}

	return &dbReport{
		PasteID: pID.String(),
		Count:   count,
		broker:  broker,
	}, nil
}

func (broker *dbBroker) GetReports() ([]model.Report, error) {
	reports := make([]model.Report, 0, 16)

	rows, err := broker.sqlDb.Query("SELECT paste_id, count FROM paste_reports")
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	for rows.Next() {
		r := &dbReport{broker: broker}
		rows.Scan(&r.PasteID, &r.Count)
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

func (broker *dbBroker) SetLoggerOption(log logrus.FieldLogger) {
	broker.Logger = log
}

func (broker *dbBroker) SetDebugOption(debug bool) {
	// no-op
}

const dbV0Schema string = `
CREATE TABLE IF NOT EXISTS _schema (
	version integer UNIQUE,
	created_at timestamp with time zone DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uix__schema_version ON _schema USING btree (version);
`

func (broker *dbBroker) migrateDb() error {
	schemaBox, err := rice.FindBox("schema")
	if err != nil {
		return err
	}

	maxVersion := -1
	schemas := make(map[int]string)
	_ = schemas
	_ = maxVersion
	err = schemaBox.Walk("" /* empty path; box is rooted at schema/ */, func(path string, fi os.FileInfo, err error) error {
		if fi.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		logrus.Info(fi.Name())

		var ver int
		var desc string
		n, _ := fmt.Sscanf(path, "%d_%s", &ver, &desc)
		if n != 2 {
			return fmt.Errorf("model.postgres: invalid schema migration filename %s", path)
		}
		schemas[ver] = path
		if ver > maxVersion {
			maxVersion = ver
		}
		return nil
	})
	if err != nil {
		return err
	}

	db := broker.sqlDb
	_, err = db.Exec(dbV0Schema)
	if err != nil {
		return err
	}

	schemaVersion := 0
	err = db.QueryRow("SELECT version FROM _schema ORDER BY version DESC LIMIT 1").Scan(&schemaVersion)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if schemaVersion > maxVersion {
		return fmt.Errorf("model.postgres: database is newer than we can support! (%d > %d)", schemaVersion, maxVersion)
	}

	logrus.Info(schemas)
	for ; schemaVersion < maxVersion; schemaVersion++ {
		tx, err := db.Begin()
		if err != nil {
			// Failed to migrate!
			return err
		}

		// we use Must, as the Walk earlier proved that these files exist.
		sch := schemaBox.MustString(schemas[schemaVersion+1])
		_, err = tx.Exec(sch)
		if err != nil {
			tx.Rollback()
			// Failed to migrate!
			return err
		}

		newVersion := schemaVersion + 1
		tx.Exec("INSERT INTO _schema(version) VALUES($1)", newVersion)

		if err := tx.Commit(); err != nil {
			tx.Rollback()
			// Failed to migrate!
			return err
		}
	}

	return nil
}

type pqDriver struct{}

func (pqDriver) Open(arguments ...interface{}) (model.Provider, error) {
	p := &dbBroker{}

	for _, arg := range arguments {
		switch a := arg.(type) {
		case *sql.DB:
			p.sqlDb = a
		case crypto.ChallengeProvider:
			p.ChallengeProvider = a
		case model.Option:
			a(p)
		default:
			return nil, fmt.Errorf("model.postgres: unknown option type %T (%v)", a, a)
		}
	}

	if p.sqlDb == nil {
		return nil, errors.New("model.postgres: no *sql.DB provided")
	}

	if p.ChallengeProvider == nil {
		return nil, errors.New("model.postgres: no ChallengeProvider provided")
	}

	db, err := gorm.Open("postgres", p.sqlDb)
	if err != nil {
		return nil, err
	}

	//db = db.Debug()
	p.DB = db

	err = p.migrateDb()
	if err != nil {
		return nil, err
	}

	res, err := p.sqlDb.Exec(
		`DELETE FROM pastes WHERE expire_at < NOW()`,
	)
	if err != nil {
		return nil, err
	}
	if p.Logger != nil {
		nrows, _ := res.RowsAffected()
		if nrows > 0 {
			p.Logger.Infof("removed %d lingering expirees", nrows)
		}
	}

	return p, nil
}

func init() {
	model.Register("postgres", &pqDriver{})
}