package service

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

func GetUserUsableGroups(userGroup string) map[string]string {
	groupsCopy := setting.GetUserUsableGroupsCopy()
	if userGroup != "" {
		specialSettings, b := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup.Get(userGroup)
		if b {
			// 处理特殊可用分组
			for specialGroup, desc := range specialSettings {
				if after, ok := strings.CutPrefix(specialGroup, "-:"); ok {
					// 移除分组
					groupToRemove := after
					delete(groupsCopy, groupToRemove)
				} else if after, ok := strings.CutPrefix(specialGroup, "+:"); ok {
					// 添加分组
					groupToAdd := after
					groupsCopy[groupToAdd] = desc
				} else {
					// 直接添加分组
					groupsCopy[specialGroup] = desc
				}
			}
		}
		// 如果userGroup不在UserUsableGroups中，返回UserUsableGroups + userGroup。
		// 描述是英文源文本，由 web 控制台翻译；管理员配置的描述原样显示。
		if _, ok := groupsCopy[userGroup]; !ok {
			groupsCopy[userGroup] = "User group"
		}
	}
	return groupsCopy
}

func GroupInUserUsableGroups(userGroup, groupName string) bool {
	_, ok := GetUserUsableGroups(userGroup)[groupName]
	return ok
}

func IsUserSelectableGroup(userGroup, groupName string) bool {
	if groupName == "" || groupName == "auto" {
		return false
	}
	return GroupInUserUsableGroups(userGroup, groupName) && ratio_setting.ContainsGroupRatio(groupName)
}

// GetUserEffectiveUsableGroups returns groups usable by the specific user:
// their static usable groups (respecting admin UserUsableGroups / special rules
// and user.Group) plus any concrete groups explicitly granted by their active
// subscriptions. The "auto" pseudo-group is included only when admin settings
// enable it (UserUsableGroups or DefaultUseAutoGroup with at least one valid
// routable auto group).
func GetUserEffectiveUsableGroups(userId int, userGroup string) map[string]string {
	groups := GetUserUsableGroups(userGroup)
	if userId > 0 {
		if subGroups, err := model.GetActiveSubscriptionAllowedGroups(userId); err == nil {
			for _, g := range subGroups {
				if !ratio_setting.ContainsGroupRatio(g) {
					continue
				}
				if _, exists := groups[g]; !exists {
					groups[g] = setting.GetUsableGroupDescription(g)
				}
			}
		}
	}
	if setting.DefaultUseAutoGroup {
		if _, exists := groups["auto"]; !exists && len(resolveEffectiveAutoGroups(groups)) > 0 {
			groups["auto"] = "Auto group"
		}
	}
	return groups
}

// UserCanUseGroup checks whether the user is authorized to route requests
// through groupName either via static usable groups, admin-enabled auto group,
// or an active subscription whose allowed_groups explicitly grants groupName.
func UserCanUseGroup(userId int, userGroup, groupName string) bool {
	trimmed := strings.TrimSpace(groupName)
	if trimmed == "" {
		return false
	}
	if GroupInUserUsableGroups(userGroup, trimmed) {
		return true
	}
	if trimmed == "auto" {
		if !setting.DefaultUseAutoGroup {
			return false
		}
		return len(GetUserEffectiveAutoGroup(userId, userGroup)) > 0
	}
	if !ratio_setting.ContainsGroupRatio(trimmed) {
		return false
	}
	ok, err := model.HasExplicitActiveSubscriptionForGroup(userId, trimmed)
	return err == nil && ok
}

func resolveEffectiveAutoGroups(effectiveUsable map[string]string) []string {
	autoGroups := make([]string, 0)
	seen := make(map[string]struct{})
	for _, group := range setting.GetAutoGroups() {
		if group == "" || group == "auto" {
			continue
		}
		if _, ok := effectiveUsable[group]; !ok || !ratio_setting.ContainsGroupRatio(group) {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		autoGroups = append(autoGroups, group)
	}
	return autoGroups
}

// GetUserEffectiveAutoGroup resolves the ordered Auto groups available to a
// user, including any group explicitly unlocked by an active subscription.
func GetUserEffectiveAutoGroup(userId int, userGroup string) []string {
	if userId <= 0 {
		return GetUserAutoGroup(userGroup)
	}
	usable := GetUserUsableGroups(userGroup)
	if subGroups, err := model.GetActiveSubscriptionAllowedGroups(userId); err == nil {
		for _, g := range subGroups {
			if ratio_setting.ContainsGroupRatio(g) {
				usable[g] = setting.GetUsableGroupDescription(g)
			}
		}
	}
	return resolveEffectiveAutoGroups(usable)
}

// GetUserAutoGroup 根据用户分组获取自动分组设置
func GetUserAutoGroup(userGroup string) []string {
	autoGroups := make([]string, 0)
	seen := make(map[string]struct{})
	for _, group := range setting.GetAutoGroups() {
		if !IsUserSelectableGroup(userGroup, group) {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		autoGroups = append(autoGroups, group)
	}
	return autoGroups
}

// FilterUserTokenAutoGroups applies current permissions before the current
// per-token limit. It intentionally does not fall back to the global Auto list.
func FilterUserTokenAutoGroups(userGroup string, groups []string) []string {
	return filterUserTokenAutoGroupsWithUser(0, userGroup, groups)
}

func filterUserTokenAutoGroupsWithUser(userId int, userGroup string, groups []string) []string {
	maxCount := setting.GetMaxTokenAutoGroups()
	filtered := make([]string, 0, min(len(groups), maxCount))
	seen := make(map[string]struct{})
	for _, group := range groups {
		if group == "" || group == "auto" || !ratio_setting.ContainsGroupRatio(group) {
			continue
		}
		allowed := IsUserSelectableGroup(userGroup, group)
		if !allowed && userId > 0 {
			if ok, err := model.HasExplicitActiveSubscriptionForGroup(userId, group); err == nil && ok {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		filtered = append(filtered, group)
		if len(filtered) == maxCount {
			break
		}
	}
	return filtered
}

// GetRequestAutoGroups resolves the ordered Auto groups for the current token.
// The absence of the context value means that the token inherits the complete
// global Auto list; a present (even empty) value is an explicit token snapshot.
func GetRequestAutoGroups(c *gin.Context, userGroup string) []string {
	userId := 0
	if c != nil {
		userId = c.GetInt("id")
	}
	value, ok := common.GetContextKey(c, constant.ContextKeyTokenAutoGroups)
	if !ok {
		return GetUserEffectiveAutoGroup(userId, userGroup)
	}
	groups, ok := value.([]string)
	if !ok {
		return []string{}
	}
	return filterUserTokenAutoGroupsWithUser(userId, userGroup, groups)
}

// GetGroupsEnabledModels 按 groups 顺序获取各分组启用的模型并去重
func GetGroupsEnabledModels(groups []string) []string {
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, group := range groups {
		for _, modelName := range model.GetGroupEnabledModels(group) {
			if _, ok := seen[modelName]; !ok {
				seen[modelName] = struct{}{}
				models = append(models, modelName)
			}
		}
	}
	return models
}

// GetUserGroupRatio 获取用户使用某个分组的倍率
// userGroup 用户分组
// group 需要获取倍率的分组
func GetUserGroupRatio(userGroup, group string) float64 {
	ratio, ok := ratio_setting.GetGroupGroupRatio(userGroup, group)
	if ok {
		return ratio
	}
	return ratio_setting.GetGroupRatio(group)
}
