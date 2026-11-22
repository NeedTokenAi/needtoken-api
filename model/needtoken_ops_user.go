package model

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var (
	ErrOpsTargetForbidden = errors.New("cannot manage admin or root account via ops user bridge")
	ErrOpsEmailTaken      = errors.New("email is already taken")
	ErrOpsUsernameTaken   = errors.New("username is already taken")
	ErrOpsInvalidStatus   = errors.New("invalid user status")
)

type OpsUserUpdateInput struct {
	Username    *string
	Email       *string
	DisplayName *string
	Group       *string
	Remark      *string
	Password    *string
	Status      *int
}

// OpsUpdateCommonUser updates a common-role user atomically under the
// cross-database email lock, enforcing email/username uniqueness and bumping
// AuthVersion whenever authentication-sensitive attributes change.
func OpsUpdateCommonUser(userID int, input OpsUserUpdateInput) (*User, bool, error) {
	if userID <= 0 {
		return nil, false, gorm.ErrRecordNotFound
	}
	var updated User
	var authChanged bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		lockEmail := ""
		if input.Email != nil {
			lockEmail = NormalizeEmail(*input.Email)
		}
		return withNormalizedEmailLock(tx, lockEmail, func(tx *gorm.DB) error {
			var current User
			if err := tx.First(&current, userID).Error; err != nil {
				return err
			}
			if current.Role >= common.RoleAdminUser {
				return ErrOpsTargetForbidden
			}

			updates := make(map[string]any)
			if input.Username != nil {
				username := strings.TrimSpace(*input.Username)
				if username != "" && username != current.Username {
					var count int64
					if err := tx.Unscoped().Model(&User{}).Where("username = ? AND id <> ?", username, userID).Count(&count).Error; err != nil {
						return err
					}
					if count > 0 {
						return ErrOpsUsernameTaken
					}
					updates["username"] = username
					authChanged = true
				}
			}

			if input.Email != nil {
				normalized := NormalizeEmail(*input.Email)
				if normalized != NormalizeEmail(current.Email) {
					if normalized != "" {
						var count int64
						if err := tx.Unscoped().Model(&User{}).Where("LOWER(email) = ? AND id <> ?", normalized, userID).Count(&count).Error; err != nil {
							return err
						}
						if count > 0 {
							return ErrOpsEmailTaken
						}
					}
					updates["email"] = normalized
					authChanged = true
				}
			}

			if input.DisplayName != nil {
				displayName := strings.TrimSpace(*input.DisplayName)
				if displayName != current.DisplayName {
					updates["display_name"] = displayName
				}
			}

			if input.Group != nil {
				group := strings.TrimSpace(*input.Group)
				if group != "" && group != current.Group {
					updates["group"] = group
					authChanged = true
				}
			}

			if input.Remark != nil && *input.Remark != current.Remark {
				updates["remark"] = *input.Remark
			}

			if input.Status != nil {
				status := *input.Status
				if status != common.UserStatusEnabled && status != common.UserStatusDisabled {
					return ErrOpsInvalidStatus
				}
				if status != current.Status {
					updates["status"] = status
					authChanged = true
				}
			}

			if input.Password != nil && *input.Password != "" {
				hashed, err := common.HashAccountPassword(*input.Password)
				if err != nil {
					return err
				}
				updates["password"] = hashed
				authChanged = true
			}

			if len(updates) > 0 {
				if err := tx.Model(&current).Updates(updates).Error; err != nil {
					return err
				}
			}
			if authChanged {
				if _, err := IncrementUserAuthVersionWithTx(tx, userID); err != nil {
					return err
				}
			}
			return tx.First(&updated, userID).Error
		})
	})
	if err != nil {
		return nil, false, err
	}
	return &updated, authChanged, nil
}
