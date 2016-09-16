package sa

import (
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/jmhodges/clock"
	jose "github.com/square/go-jose"
	"golang.org/x/net/context"
	gorp "gopkg.in/gorp.v1"

	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/revocation"
)

// SQLStorageAuthority defines a Storage Authority
type SQLStorageAuthority struct {
	dbMap *gorp.DbMap
	clk   clock.Clock
	log   blog.Logger
}

func digest256(data []byte) []byte {
	d := sha256.New()
	_, _ = d.Write(data) // Never returns an error
	return d.Sum(nil)
}

// Utility models
type pendingauthzModel struct {
	core.Authorization

	LockCol int64
}

type authzModel struct {
	core.Authorization
}

// NewSQLStorageAuthority provides persistence using a SQL backend for
// Boulder. It will modify the given gorp.DbMap by adding relevant tables.
func NewSQLStorageAuthority(dbMap *gorp.DbMap, clk clock.Clock, logger blog.Logger) (*SQLStorageAuthority, error) {
	SetSQLDebug(dbMap, logger)

	ssa := &SQLStorageAuthority{
		dbMap: dbMap,
		clk:   clk,
		log:   logger,
	}

	return ssa, nil
}

func existingRegistration(tx *gorp.Transaction, id int64) bool {
	var count int64
	_ = tx.SelectOne(&count, "SELECT count(*) FROM registrations WHERE id = :id", map[string]interface{}{"id": id})
	return count > 0
}

func updateChallenges(authID string, challenges []core.Challenge, tx *gorp.Transaction) error {
	var challs []challModel
	_, err := tx.Select(
		&challs,
		getChallengesQuery,
		map[string]interface{}{"authID": authID},
	)
	if err != nil {
		return err
	}
	if len(challs) != len(challenges) {
		return fmt.Errorf("Invalid number of challenges provided")
	}
	for i, authChall := range challenges {
		chall, err := challengeToModel(&authChall, challs[i].AuthorizationID)
		if err != nil {
			return err
		}
		chall.ID = challs[i].ID
		_, err = tx.Update(chall)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetRegistration obtains a Registration by ID
func (ssa *SQLStorageAuthority) GetRegistration(ctx context.Context, id int64) (core.Registration, error) {
	regObj, err := ssa.dbMap.Get(regModel{}, id)
	if err != nil {
		return core.Registration{}, err
	}
	if regObj == nil {
		msg := fmt.Sprintf("No registrations with ID %d", id)
		return core.Registration{}, core.NoSuchRegistrationError(msg)
	}
	regPtr, ok := regObj.(*regModel)
	if !ok {
		return core.Registration{}, fmt.Errorf("Invalid cast to reg model object")
	}
	return modelToRegistration(regPtr)
}

// GetRegistrationByKey obtains a Registration by JWK
func (ssa *SQLStorageAuthority) GetRegistrationByKey(ctx context.Context, key jose.JsonWebKey) (core.Registration, error) {
	reg := &regModel{}
	sha, err := core.KeyDigest(key.Key)
	if err != nil {
		return core.Registration{}, err
	}
	err = ssa.dbMap.SelectOne(reg, "SELECT * FROM registrations WHERE jwk_sha256 = :key", map[string]interface{}{"key": sha})

	if err == sql.ErrNoRows {
		msg := fmt.Sprintf("No registrations with public key sha256 %s", sha)
		return core.Registration{}, core.NoSuchRegistrationError(msg)
	}
	if err != nil {
		return core.Registration{}, err
	}

	return modelToRegistration(reg)
}

// GetAuthorization obtains an Authorization by ID
func (ssa *SQLStorageAuthority) GetAuthorization(ctx context.Context, id string) (core.Authorization, error) {
	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return core.Authorization{}, err
	}

	authz, _, err := getAuthz(tx, id)
	if err != nil {
		err = Rollback(tx, err)
		return authz, err
	}

	err = tx.Commit()
	return authz, err
}

// GetValidAuthorizations returns the latest authorization object for all
// domain names from the parameters that the account has authorizations for.
func (ssa *SQLStorageAuthority) GetValidAuthorizations(ctx context.Context, registrationID int64, names []string, now time.Time) (latest map[string]*core.Authorization, err error) {
	if len(names) == 0 {
		return nil, errors.New("GetValidAuthorizations: no names received")
	}

	params := make([]interface{}, len(names))
	qmarks := make([]string, len(names))
	for i, name := range names {
		id := core.AcmeIdentifier{Type: core.IdentifierDNS, Value: name}
		idJSON, err := json.Marshal(id)
		if err != nil {
			return nil, err
		}
		params[i] = string(idJSON)
		qmarks[i] = "?"
	}

	var auths []*core.Authorization
	_, err = ssa.dbMap.Select(&auths, `
		SELECT * FROM authz
		WHERE registrationID = ?
		AND expires > ?
		AND identifier IN (`+strings.Join(qmarks, ",")+`)
		AND status = 'valid'
		`, append([]interface{}{registrationID, now}, params...)...)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]*core.Authorization)
	for _, auth := range auths {
		// No real life authorizations should have a nil expires. If we find them,
		// don't consider them valid.
		if auth.Expires == nil {
			continue
		}
		if auth.Identifier.Type != core.IdentifierDNS {
			return nil, fmt.Errorf("unknown identifier type: %q on authz id %q", auth.Identifier.Type, auth.ID)
		}
		existing, present := byName[auth.Identifier.Value]
		if !present || auth.Expires.After(*existing.Expires) {
			byName[auth.Identifier.Value] = auth
		}
	}

	return byName, nil
}

// incrementIP returns a copy of `ip` incremented at a bit index `index`,
// or in other words the first IP of the next highest subnet given a mask of
// length `index`.
// In order to easily account for overflow, we treat ip as a big.Int and add to
// it. If the increment overflows the max size of a net.IP, return the highest
// possible net.IP.
func incrementIP(ip net.IP, index int) net.IP {
	bigInt := new(big.Int)
	bigInt.SetBytes([]byte(ip))
	incr := new(big.Int).Lsh(big.NewInt(1), 128-uint(index))
	bigInt.Add(bigInt, incr)
	// bigInt.Bytes can be shorter than 16 bytes, so stick it into a
	// full-sized net.IP.
	resultBytes := bigInt.Bytes()
	if len(resultBytes) > 16 {
		return net.ParseIP("ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	}
	result := make(net.IP, 16)
	copy(result[16-len(resultBytes):], resultBytes)
	return result
}

// ipRange returns a range of IP addresses suitable for querying MySQL for the
// purpose of rate limiting using a range that is inclusive on the lower end and
// exclusive at the higher end. If ip is an IPv4 address, it returns that address,
// plus the one immediately higher than it. If ip is an IPv6 address, it applies
// a /48 mask to it and returns the lowest IP in the resulting network, and the
// first IP outside of the resulting network.
func ipRange(ip net.IP) (net.IP, net.IP) {
	ip = ip.To16()
	// For IPv6, match on a certain subnet range, since one person can commonly
	// have an entire /48 to themselves.
	maskLength := 48
	// For IPv4 addresses, do a match on exact address, so begin = ip and end =
	// next higher IP.
	if ip.To4() != nil {
		maskLength = 128
	}

	mask := net.CIDRMask(maskLength, 128)
	begin := ip.Mask(mask)
	end := incrementIP(begin, maskLength)

	return begin, end
}

// CountRegistrationsByIP returns the number of registrations created in the
// time range in an IP range. For IPv4 addresses, that range is limited to the
// single IP. For IPv6 addresses, that range is a /48, since it's not uncommon
// for one person to have a /48 to themselves.
func (ssa *SQLStorageAuthority) CountRegistrationsByIP(ctx context.Context, ip net.IP, earliest time.Time, latest time.Time) (int, error) {
	var count int64
	beginIP, endIP := ipRange(ip)
	err := ssa.dbMap.SelectOne(
		&count,
		`SELECT COUNT(1) FROM registrations
		 WHERE 
		 :beginIP <= initialIP AND
		 initialIP < :endIP AND
		 :earliest < createdAt AND
		 createdAt <= :latest`,
		map[string]interface{}{
			"ip":       ip.String(),
			"earliest": earliest,
			"latest":   latest,
			"beginIP":  []byte(beginIP),
			"endIP":    []byte(endIP),
		})
	if err != nil {
		return -1, err
	}
	return int(count), nil
}

// TooManyCertificatesError indicates that the number of certificates returned by
// CountCertificates exceeded the hard-coded limit of 10,000 certificates.
type TooManyCertificatesError string

func (t TooManyCertificatesError) Error() string {
	return string(t)
}

// CountCertificatesByNames counts, for each input domain, the number of
// certificates issued in the given time range for that domain and its
// subdomains. It returns a map from domains to counts, which is guaranteed to
// contain an entry for each input domain, so long as err is nil.
// The highest count this function can return is 10,000. If there are more
// certificates than that matching one of the provided domain names, it will return
// TooManyCertificatesError.
func (ssa *SQLStorageAuthority) CountCertificatesByNames(ctx context.Context, domains []string, earliest, latest time.Time) (map[string]int, error) {
	ret := make(map[string]int, len(domains))
	for _, domain := range domains {
		currentCount, err := ssa.countCertificatesByName(domain, earliest, latest)
		if err != nil {
			return ret, err
		}
		ret[domain] = currentCount
	}
	return ret, nil
}

// countCertificatesByNames returns, for a single domain, the count of
// certificates issued in the given time range for that domain and its
// subdomains.
// The highest count this function can return is 10,000. If there are more
// certificates than that matching one of the provided domain names, it will return
// TooManyCertificatesError.
func (ssa *SQLStorageAuthority) countCertificatesByName(domain string, earliest, latest time.Time) (int, error) {
	var count int64
	const max = 10000
	var serials []struct {
		Serial string
	}
	_, err := ssa.dbMap.Select(
		&serials,
		`SELECT serial from issuedNames
		 WHERE (reversedName = :reversedDomain OR
			      reversedName LIKE CONCAT(:reversedDomain, ".%"))
		 AND notBefore > :earliest AND notBefore <= :latest
		 LIMIT :limit;`,
		map[string]interface{}{
			"reversedDomain": core.ReverseName(domain),
			"earliest":       earliest,
			"latest":         latest,
			"limit":          max + 1,
		})
	if err == sql.ErrNoRows {
		return 0, nil
	} else if err != nil {
		return -1, err
	} else if count > max {
		return max, TooManyCertificatesError(fmt.Sprintf("More than %d issuedName entries for %s.", max, domain))
	}
	serialMap := make(map[string]struct{}, len(serials))
	for _, s := range serials {
		serialMap[s.Serial] = struct{}{}
	}

	return len(serialMap), nil
}

// GetCertificate takes a serial number and returns the corresponding
// certificate, or error if it does not exist.
func (ssa *SQLStorageAuthority) GetCertificate(ctx context.Context, serial string) (core.Certificate, error) {
	if !core.ValidSerial(serial) {
		err := fmt.Errorf("Invalid certificate serial %s", serial)
		return core.Certificate{}, err
	}

	certObj, err := ssa.dbMap.Get(core.Certificate{}, serial)
	if err != nil {
		return core.Certificate{}, err
	}
	if certObj == nil {
		ssa.log.Debug(fmt.Sprintf("Nil cert for %s", serial))
		return core.Certificate{}, core.NotFoundError(fmt.Sprintf("No certificate found for %s", serial))
	}

	certPtr, ok := certObj.(*core.Certificate)
	if !ok {
		ssa.log.Debug("Failed to convert cert")
		return core.Certificate{}, fmt.Errorf("Error converting certificate response for %s", serial)
	}
	return *certPtr, err
}

// GetCertificateStatus takes a hexadecimal string representing the full 128-bit serial
// number of a certificate and returns data about that certificate's current
// validity.
func (ssa *SQLStorageAuthority) GetCertificateStatus(ctx context.Context, serial string) (status core.CertificateStatus, err error) {
	if !core.ValidSerial(serial) {
		err := fmt.Errorf("Invalid certificate serial %s", serial)
		return core.CertificateStatus{}, err
	}

	certificateStats, err := ssa.dbMap.Get(core.CertificateStatus{}, serial)
	if err != nil {
		return
	}
	status = *certificateStats.(*core.CertificateStatus)
	return
}

// NewRegistration stores a new Registration
func (ssa *SQLStorageAuthority) NewRegistration(ctx context.Context, reg core.Registration) (core.Registration, error) {
	rm, err := registrationToModel(&reg)
	if err != nil {
		return reg, err
	}
	rm.CreatedAt = ssa.clk.Now()
	err = ssa.dbMap.Insert(rm)
	if err != nil {
		return reg, err
	}
	return modelToRegistration(rm)
}

// MarkCertificateRevoked stores the fact that a certificate is revoked, along
// with a timestamp and a reason.
func (ssa *SQLStorageAuthority) MarkCertificateRevoked(ctx context.Context, serial string, reasonCode revocation.Reason) (err error) {
	if _, err = ssa.GetCertificate(ctx, serial); err != nil {
		return fmt.Errorf(
			"Unable to mark certificate %s revoked: cert not found.", serial)
	}

	if _, err = ssa.GetCertificateStatus(ctx, serial); err != nil {
		return fmt.Errorf(
			"Unable to mark certificate %s revoked: cert status not found.", serial)
	}

	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return
	}

	statusObj, err := tx.Get(core.CertificateStatus{}, serial)
	if err != nil {
		err = Rollback(tx, err)
		return
	}
	if statusObj == nil {
		err = fmt.Errorf("No certificate with serial %s", serial)
		err = Rollback(tx, err)
		return
	}
	now := ssa.clk.Now()
	status := statusObj.(*core.CertificateStatus)
	status.Status = core.OCSPStatusRevoked
	status.RevokedDate = now
	status.RevokedReason = reasonCode

	n, err := tx.Update(status)
	if err != nil {
		err = Rollback(tx, err)
		return
	}
	if n == 0 {
		err = Rollback(tx, err)
		err = errors.New("No certificate updated. Maybe the lock column was off?")
		return
	}
	err = tx.Commit()

	return
}

// UpdateRegistration stores an updated Registration
func (ssa *SQLStorageAuthority) UpdateRegistration(ctx context.Context, reg core.Registration) error {
	lookupResult, err := ssa.dbMap.Get(regModel{}, reg.ID)
	if err != nil {
		return err
	}
	if lookupResult == nil {
		msg := fmt.Sprintf("No registrations with ID %d", reg.ID)
		return core.NoSuchRegistrationError(msg)
	}
	existingRegModel, ok := lookupResult.(*regModel)
	if !ok {
		// Shouldn't happen
		return fmt.Errorf("Incorrect type returned from registration lookup")
	}

	updatedRegModel, err := registrationToModel(&reg)
	if err != nil {
		return err
	}
	updatedRegModel.LockCol = existingRegModel.LockCol

	n, err := ssa.dbMap.Update(updatedRegModel)
	if err != nil {
		return err
	}
	if n == 0 {
		msg := fmt.Sprintf("Requested registration not found %d", reg.ID)
		return core.NoSuchRegistrationError(msg)
	}

	return nil
}

// NewPendingAuthorization stores a new Pending Authorization
func (ssa *SQLStorageAuthority) NewPendingAuthorization(ctx context.Context, authz core.Authorization) (core.Authorization, error) {
	var result core.Authorization

	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return result, err
	}

	authz.ID = core.NewToken()
	// Check that the generated ID doesn't exist already, creating new IDs until
	// one that doesn't exist is found.
	for authzIdExists(tx, authz.ID) {
		authz.ID = core.NewToken()
	}

	// Historically it didn't matter if the caller provided a status or not,
	// putting a row in the `pendingAuthorizations` table was sufficient for it to
	// be pending. Since we now insert the authz row into the `authz` table we
	// need to explicitly set the status to `core.StatusPending` before creating
	// a row in the authz table.
	authz.Status = core.StatusPending

	// Insert a stub row in the authz table
	pendingAuthz := authzModel{Authorization: authz}
	err = tx.Insert(&pendingAuthz)
	if err != nil {
		err = Rollback(tx, err)
		return result, err
	}

	for i, c := range authz.Challenges {
		challModel, err := challengeToModel(&c, pendingAuthz.ID)
		if err != nil {
			err = Rollback(tx, err)
			return result, err
		}
		// Magic happens here: Gorp will modify challModel, setting challModel.ID
		// to the auto-increment primary key. This is important because we want
		// the challenge objects inside the Authorization we return to know their
		// IDs, so they can have proper URLs.
		// See https://godoc.org/github.com/coopernurse/gorp#DbMap.Insert
		err = tx.Insert(challModel)
		if err != nil {
			err = Rollback(tx, err)
			return result, err
		}
		challenge, err := modelToChallenge(challModel)
		if err != nil {
			err = Rollback(tx, err)
			return result, err
		}
		authz.Challenges[i] = challenge
	}

	err = tx.Commit()
	result = pendingAuthz.Authorization
	result.Challenges = authz.Challenges
	return result, err
}

// UpdatePendingAuthorization updates a Pending Authorization
func (ssa *SQLStorageAuthority) UpdatePendingAuthorization(ctx context.Context, authz core.Authorization) error {
	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return err
	}

	// If the provided authz isn't Status: pending that's a problem, return early.
	if !statusIsPending(authz.Status) {
		err = errors.New("Use FinalizeAuthorization() to update to a final status")
		err = Rollback(tx, err)
		return err
	}

	dbAuthz, table, err := getAuthz(tx, authz.ID)
	if err != nil {
		err = Rollback(tx, err)
		return err
	}

	// If the existing authz row isn't pending, we can't update it
	if !statusIsPending(dbAuthz.Status) {
		err = errors.New("Cannot update a non-pending authorization")
		err = Rollback(tx, err)
		return err
	}

	var updateAuth interface{}
	if table == "pendingAuthorization" {
		// If the authz came from the legacy pending table, use
		// a `pendingAuthzModel` as the `updateAuth`.
		updateAuth = &pendingauthzModel{Authorization: authz}
	} else if table == "authz" {
		// If the authz came from the authz table, use an authzModel
		updateAuth = &authzModel{Authorization: authz}
	} else {
		// Should never happen - we only have two fixed authz tables!
		err = errors.New("Internal error. Table mismatch updating authz")
		err = Rollback(tx, err)
		return err
	}

	_, err = tx.Update(updateAuth)
	if err != nil {
		err = Rollback(tx, err)
		return err
	}
	err = updateChallenges(authz.ID, authz.Challenges, tx)
	if err != nil {
		err = Rollback(tx, err)
		return err
	}

	err = tx.Commit()
	return err
}

// FinalizeAuthorization converts a Pending Authorization to a final one
func (ssa *SQLStorageAuthority) FinalizeAuthorization(ctx context.Context, authz core.Authorization) error {
	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return err
	}

	dbAuthz, table, err := getAuthz(tx, authz.ID)
	if err != nil {
		err = Rollback(tx, err)
		return err
	}

	// If the existing authz from the DB isn't currently pending, we can't finalize it
	if !statusIsPending(dbAuthz.Status) {
		err = errors.New("Cannot finalize an authorization that is not pending")
		err = Rollback(tx, err)
		return err
	}

	// If the authz update is to a pending status, we can't do that! Use
	// `UpdatePendingAuthorization`
	if statusIsPending(authz.Status) {
		err = errors.New("Cannot finalize an authorization to a non-final status")
		err = Rollback(tx, err)
		return err
	}

	// If we found a pending authz in the pendingAuthorizations table, follow
	// the legacy finalization process: insert a new final `authz` row, delete the
	// old `pendingAuthorizations` row
	if table == "pendingAuthorizations" {
		oldRow := pendingauthzModel{Authorization: dbAuthz}
		newRow := &authzModel{authz}

		err = tx.Insert(newRow)
		if err != nil {
			err = Rollback(tx, err)
			return err
		}

		_, err = tx.Delete(oldRow)
		if err != nil {
			err = Rollback(tx, err)
			return err
		}
	} else if table == "authz" {
		// Otherwise, for a pending authz found in the authz table we can just
		// UPDATE the existing authz row.
		updatedRow := &authzModel{authz}
		_, err = tx.Update(updatedRow)
		if err != nil {
			err = Rollback(tx, err)
			return err
		}
	} else {
		// Should not happen! There are only two tables defined
		// `authorizationTables` from `sa/authz.go`
		err = errors.New("Internal error finalizing authz from unknown table")
		err = Rollback(tx, err)
		return err
	}

	err = updateChallenges(authz.ID, authz.Challenges, tx)
	if err != nil {
		err = Rollback(tx, err)
		return err
	}

	err = tx.Commit()
	return err
}

// RevokeAuthorizationsByDomain invalidates all pending or finalized authorizations
// for a specific domain
func (ssa *SQLStorageAuthority) RevokeAuthorizationsByDomain(ctx context.Context, ident core.AcmeIdentifier) (int64, int64, error) {
	identifierJSON, err := json.Marshal(ident)
	if err != nil {
		return 0, 0, err
	}
	identifier := string(identifierJSON)
	results := []int64{0, 0}

	now := ssa.clk.Now()
	for _, table := range authorizationTables {
		for {
			authz, err := getAuthorizationIDsByDomain(ssa.dbMap, table, identifier, now)
			if err != nil {
				return results[0], results[1], err
			}
			numAuthz := len(authz)
			if numAuthz == 0 {
				break
			}

			numRevoked, err := revokeAuthorizations(ssa.dbMap, table, authz)
			if err != nil {
				return results[0], results[1], err
			}

			//
			if table == "pendingAuthorizations" {
				results[0] += numRevoked
			} else if table == "authz" {
				results[1] += numRevoked
			} else {
				// Shouldn't ever happen! Only two authz tables exist.
				return results[0], results[1], fmt.Errorf("Internal error: unknown authz table")
			}

			if numRevoked < int64(numAuthz) {
				return results[0], results[1], fmt.Errorf("Didn't revoke all found authorizations")
			}
		}
	}

	return results[0], results[1], nil
}

// AddCertificate stores an issued certificate.
func (ssa *SQLStorageAuthority) AddCertificate(ctx context.Context, certDER []byte, regID int64) (digest string, err error) {
	var parsedCertificate *x509.Certificate
	parsedCertificate, err = x509.ParseCertificate(certDER)
	if err != nil {
		return
	}
	digest = core.Fingerprint256(certDER)
	serial := core.SerialToString(parsedCertificate.SerialNumber)

	cert := &core.Certificate{
		RegistrationID: regID,
		Serial:         serial,
		Digest:         digest,
		DER:            certDER,
		Issued:         ssa.clk.Now(),
		Expires:        parsedCertificate.NotAfter,
	}
	certStatus := &core.CertificateStatus{
		SubscriberApproved: false,
		Status:             core.OCSPStatus("good"),
		OCSPLastUpdated:    time.Time{},
		OCSPResponse:       []byte{},
		Serial:             serial,
		RevokedDate:        time.Time{},
		RevokedReason:      0,
		LockCol:            0,
	}

	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return
	}

	// TODO Verify that the serial number doesn't yet exist
	err = tx.Insert(cert)
	if err != nil {
		err = Rollback(tx, err)
		return
	}

	err = tx.Insert(certStatus)
	if err != nil {
		err = Rollback(tx, err)
		return
	}

	err = addIssuedNames(tx, parsedCertificate)
	if err != nil {
		err = Rollback(tx, err)
		return
	}

	err = addFQDNSet(
		tx,
		parsedCertificate.DNSNames,
		serial,
		parsedCertificate.NotBefore,
		parsedCertificate.NotAfter,
	)
	if err != nil {
		err = Rollback(tx, err)
		return
	}

	err = tx.Commit()
	return
}

// CountCertificatesRange returns the number of certificates issued in a specific
// date range
func (ssa *SQLStorageAuthority) CountCertificatesRange(ctx context.Context, start, end time.Time) (count int64, err error) {
	err = ssa.dbMap.SelectOne(
		&count,
		`SELECT COUNT(1) FROM certificates
		WHERE issued >= :windowLeft
		AND issued < :windowRight`,
		map[string]interface{}{
			"windowLeft":  start,
			"windowRight": end,
		},
	)
	return count, err
}

// CountPendingAuthorizations returns the number of pending, unexpired
// authorizations for the given registration.
func (ssa *SQLStorageAuthority) CountPendingAuthorizations(ctx context.Context, regID int64) (int, error) {
	var count int64

	/*
	 * We need to look at *both* the `authz` table and the `pendingAuthorizations`
	 * table during the transition period described in Issue 2162[0]
	 *
	 * [0] - https://github.com/letsencrypt/boulder/issues/2162
	 */
	for _, table := range authorizationTables {
		stmtArgs := []interface{}{regID, ssa.clk.Now()}
		qmarks := []string{}
		for _, status := range pendingStatuses {
			stmtArgs = append(stmtArgs, string(status))
			qmarks = append(qmarks, "?")
		}
		statusStmt := fmt.Sprintf("(%s)", strings.Join(qmarks, ","))

		var tableCount int64
		err := ssa.dbMap.SelectOne(&tableCount, fmt.Sprintf(`
		SELECT COUNT(1) FROM %s
		WHERE registrationID = ?
		AND expires > ?
		AND status IN %s`, table, statusStmt),
			stmtArgs...)
		if err != nil {
			return int(count), nil
		}
		count += tableCount
	}

	return int(count), nil
}

// ErrNoReceipt is an error type for non-existent SCT receipt
type ErrNoReceipt string

func (e ErrNoReceipt) Error() string {
	return string(e)
}

// GetSCTReceipt gets a specific SCT receipt for a given certificate serial and
// CT log ID
func (ssa *SQLStorageAuthority) GetSCTReceipt(ctx context.Context, serial string, logID string) (receipt core.SignedCertificateTimestamp, err error) {
	err = ssa.dbMap.SelectOne(
		&receipt,
		"SELECT * FROM sctReceipts WHERE certificateSerial = :serial AND logID = :logID",
		map[string]interface{}{
			"serial": serial,
			"logID":  logID,
		},
	)

	if err == sql.ErrNoRows {
		err = ErrNoReceipt(err.Error())
		return
	}

	return
}

// AddSCTReceipt adds a new SCT receipt to the (append-only) sctReceipts table
func (ssa *SQLStorageAuthority) AddSCTReceipt(ctx context.Context, sct core.SignedCertificateTimestamp) error {
	err := ssa.dbMap.Insert(&sct)
	// For AddSCTReceipt, duplicates are explicitly OK, so don't return errors
	// based on duplicates, especially because we currently retry all submissions
	// for a certificate if even one of them fails. Once https://github.com/letsencrypt/boulder/issues/891
	// is fixed, we may want to start returning this as an error, or logging it.
	if err != nil && strings.HasPrefix(err.Error(), "Error 1062: Duplicate entry") {
		return nil
	}
	return err
}

func hashNames(names []string) []byte {
	names = core.UniqueLowerNames(names)
	hash := sha256.Sum256([]byte(strings.Join(names, ",")))
	return hash[:]
}

func addFQDNSet(tx *gorp.Transaction, names []string, serial string, issued time.Time, expires time.Time) error {
	return tx.Insert(&core.FQDNSet{
		SetHash: hashNames(names),
		Serial:  serial,
		Issued:  issued,
		Expires: expires,
	})
}

type execable interface {
	Exec(string, ...interface{}) (sql.Result, error)
}

func addIssuedNames(tx execable, cert *x509.Certificate) error {
	var qmarks []string
	var values []interface{}
	for _, name := range cert.DNSNames {
		values = append(values,
			core.ReverseName(name),
			core.SerialToString(cert.SerialNumber),
			cert.NotBefore)
		qmarks = append(qmarks, "(?, ?, ?)")
	}
	query := `INSERT INTO issuedNames (reversedName, serial, notBefore) VALUES ` + strings.Join(qmarks, ", ") + `;`
	_, err := tx.Exec(query, values...)
	return err
}

// CountFQDNSets returns the number of sets with hash |setHash| within the window
// |window|
func (ssa *SQLStorageAuthority) CountFQDNSets(ctx context.Context, window time.Duration, names []string) (int64, error) {
	var count int64
	err := ssa.dbMap.SelectOne(
		&count,
		`SELECT COUNT(1) FROM fqdnSets
		WHERE setHash = ?
		AND issued > ?`,
		hashNames(names),
		ssa.clk.Now().Add(-window),
	)
	return count, err
}

// FQDNSetExists returns a bool indicating if one or more FQDN sets |names|
// exists in the database
func (ssa *SQLStorageAuthority) FQDNSetExists(ctx context.Context, names []string) (bool, error) {
	var count int64
	err := ssa.dbMap.SelectOne(
		&count,
		`SELECT COUNT(1) FROM fqdnSets
		WHERE setHash = ?
		LIMIT 1`,
		hashNames(names),
	)
	return count > 0, err
}

// DeactivateAuthorization deactivates a currently valid or pending authorization
func (ssa *SQLStorageAuthority) DeactivateAuthorization(ctx context.Context, id string) error {
	tx, err := ssa.dbMap.Begin()
	if err != nil {
		return err
	}

	authz, table, err := getAuthz(tx, id)
	if err != nil {
		return err
	}

	if authz.Status != core.StatusPending && authz.Status != core.StatusValid {
		return nil
	}

	// Note: we use the `table` returned from `getAuthz` to update a row in the
	//   `pendingAuthorizations` or `authz` as appropriate.
	_, err = tx.Exec(
		fmt.Sprintf(`UPDATE %s SET status = ? WHERE id = ? and status IN (?, ?)`, table),
		string(core.StatusDeactivated), id, string(core.StatusPending), string(core.StatusValid))
	if err != nil {
		err = Rollback(tx, err)
		return err
	}
	return tx.Commit()
}
