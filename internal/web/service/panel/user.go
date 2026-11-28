package panel

import (
	"errors"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	ldaputil "github.com/Dark-Sky07/phoenix/internal/util/ldap"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/xlzd/gotp"
	"gorm.io/gorm"
)

// UserService provides business logic for user management and authentication.
// It handles user creation, login, password management, and 2FA operations.
type UserService struct {
	settingService service.SettingService
}

// GetFirstUser retrieves the first user from the database.
// This is typically used for initial setup or when there's only one admin user.
func (s *UserService) GetFirstUser() (*model.User, error) {
	db := database.GetDB()

	user := &model.User{}
	err := db.Model(model.User{}).
		First(user).
		Error
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (s *UserService) CheckUser(username string, password string, twoFactorCode string) (*model.User, error) {
	db := database.GetDB()

	user := &model.User{}

	err := db.Model(model.User{}).
		Where("username = ?", username).
		First(user).
		Error
	if err == gorm.ErrRecordNotFound {
		return nil, errors.New("invalid credentials")
	} else if err != nil {
		logger.Warning("check user err:", err)
		return nil, err
	}

	if !crypto.CheckPasswordHash(user.Password, password) {
		ldapEnabled, _ := s.settingService.GetLdapEnable()
		if !ldapEnabled {
			return nil, errors.New("invalid credentials")
		}

		host, _ := s.settingService.GetLdapHost()
		port, _ := s.settingService.GetLdapPort()
		useTLS, _ := s.settingService.GetLdapUseTLS()
		bindDN, _ := s.settingService.GetLdapBindDN()
		ldapPass, _ := s.settingService.GetLdapPassword()
		baseDN, _ := s.settingService.GetLdapBaseDN()
		userFilter, _ := s.settingService.GetLdapUserFilter()
		userAttr, _ := s.settingService.GetLdapUserAttr()

		cfg := ldaputil.Config{
			Host:       host,
			Port:       port,
			UseTLS:     useTLS,
			BindDN:     bindDN,
			Password:   ldapPass,
			BaseDN:     baseDN,
			UserFilter: userFilter,
			UserAttr:   userAttr,
		}
		ok, err := ldaputil.AuthenticateUser(cfg, username, password)
		if err != nil || !ok {
			return nil, errors.New("invalid credentials")
		}
	}

	twoFactorEnable, err := s.settingService.GetTwoFactorEnable()
	if err != nil {
		logger.Warning("check two factor err:", err)
		return nil, err
	}

	if twoFactorEnable {
		twoFactorToken, err := s.settingService.GetTwoFactorToken()

		if err != nil {
			logger.Warning("check two factor token err:", err)
			return nil, err
		}

		if gotp.NewDefaultTOTP(twoFactorToken).Now() != twoFactorCode {
			// Recovery codes are high-entropy, per-admin, and atomically
			// consumed. They are checked only after the password succeeds.
			if !consumeRecoveryCode(user.Id, twoFactorCode) {
				return nil, errors.New("invalid 2fa code")
			}
		}
	}

	if user.Disabled && user.Id != 1 {
		return nil, errors.New("invalid credentials")
	}
	now := time.Now().UnixMilli()
	_ = db.Model(&model.User{}).Where("id = ?", user.Id).Update("last_login_at", now).Error
	user.LastLoginAt = now

	return user, nil
}

func (s *UserService) BumpLoginEpoch() error {
	db := database.GetDB()
	return db.Model(model.User{}).
		Where("1 = 1").
		Update("login_epoch", gorm.Expr("login_epoch + 1")).
		Error
}

func (s *UserService) UpdateUser(id int, username string, password string) error {
	db := database.GetDB()
	hashedPassword, err := crypto.HashPasswordAsBcrypt(password)

	if err != nil {
		return err
	}

	twoFactorEnable, err := s.settingService.GetTwoFactorEnable()
	if err != nil {
		return err
	}

	if twoFactorEnable {
		s.settingService.SetTwoFactorEnable(false)
		s.settingService.SetTwoFactorToken("")
	}

	return db.Model(model.User{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"username":    username,
			"password":    hashedPassword,
			"login_epoch": gorm.Expr("login_epoch + 1"),
		}).
		Error
}

func (s *UserService) UpdateFirstUser(username string, password string) error {
	if username == "" {
		return errors.New("username can not be empty")
	} else if password == "" {
		return errors.New("password can not be empty")
	}
	hashedPassword, er := crypto.HashPasswordAsBcrypt(password)

	if er != nil {
		return er
	}

	db := database.GetDB()
	user := &model.User{}
	err := db.Model(model.User{}).First(user).Error
	if database.IsNotFound(err) {
		user.Username = username
		user.Password = hashedPassword
		return db.Model(model.User{}).Create(user).Error
	} else if err != nil {
		return err
	}
	user.Username = username
	user.Password = hashedPassword
	user.LoginEpoch++
	return db.Save(user).Error
}

const recoveryCodeCount = 10

// GenerateRecoveryCodes invalidates the caller's previous unused recovery
// codes and returns new values exactly once. Only SHA-256 digests are stored.
func (s *UserService) GenerateRecoveryCodes(userID int) ([]string, error) {
	if userID <= 0 {
		return nil, errors.New("invalid user")
	}
	db := database.GetDB()
	codes := make([]string, recoveryCodeCount)
	rows := make([]model.RecoveryCode, recoveryCodeCount)
	for i := range codes {
		codes[i] = random.NumLower(20)
		rows[i] = model.RecoveryCode{UserID: userID, CodeHash: crypto.HashTokenSHA256(codes[i])}
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND used_at = ?", userID, 0).Delete(&model.RecoveryCode{}).Error; err != nil {
			return err
		}
		return tx.Create(&rows).Error
	}); err != nil {
		return nil, err
	}
	return codes, nil
}

func consumeRecoveryCode(userID int, code string) bool {
	code = strings.TrimSpace(strings.ToLower(code))
	if userID <= 0 || code == "" {
		return false
	}
	now := time.Now().UnixMilli()
	result := database.GetDB().Model(&model.RecoveryCode{}).
		Where("user_id = ? AND code_hash = ? AND used_at = ?", userID, crypto.HashTokenSHA256(code), 0).
		Update("used_at", now)
	return result.Error == nil && result.RowsAffected == 1
}
