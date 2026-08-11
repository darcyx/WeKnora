package handler

import (
	"context"
	"net/http"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// usableSkillLister returns the installed skills a chat turn can actually
// invoke on one sandbox config. The @ picker and the agent editor both read
// this set so they cannot offer a skill the running image does not carry.
type usableSkillLister interface {
	ListUsableSkills(ctx context.Context, tenantID uint64, configID string) []*types.TenantSkillEntity
}

// SkillHandler handles skill-related HTTP requests
type SkillHandler struct {
	preloadedSkills interfaces.SkillService
	usableSkills    usableSkillLister
}

// NewSkillHandler creates a new skill handler
func NewSkillHandler(
	preloadedSkills interfaces.SkillService,
	usableSkills usableSkillLister,
) *SkillHandler {
	return &SkillHandler{
		preloadedSkills: preloadedSkills,
		usableSkills:    usableSkills,
	}
}

// SkillInfoResponse represents the skill info returned to frontend
type SkillInfoResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListSkills godoc
// @Summary      获取当前沙箱配置上可执行的 Skills
// @Description  返回指定沙箱配置镜像内、智能体实际能调用的已安装技能（ready 且启用）。不传 sandbox_config_id 时列表为空。
// @Tags         Skills
// @Accept       json
// @Produce      json
// @Param        sandbox_config_id  query     string  false  "Sandbox config ID"
// @Success      200  {object}  map[string]interface{}  "Skills列表"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /skills [get]
func (h *SkillHandler) ListSkills(c *gin.Context) {
	configID := c.Query("sandbox_config_id")
	response := make([]SkillInfoResponse, 0)
	indexByName := make(map[string]int)

	if h.preloadedSkills != nil {
		metadata, err := h.preloadedSkills.ListPreloadedSkills(c.Request.Context())
		if err != nil {
			logger.ErrorWithFields(c.Request.Context(), err, nil)
			c.Error(apperrors.NewInternalServerError("failed to list preloaded skills: " + err.Error()))
			return
		}
		for _, meta := range metadata {
			if meta == nil || meta.Name == "" {
				continue
			}
			indexByName[meta.Name] = len(response)
			response = append(response, SkillInfoResponse{
				Name:        meta.Name,
				Description: meta.Description,
			})
		}
	}

	if configID != "" && h.usableSkills != nil {
		rows := h.usableSkills.ListUsableSkills(
			c.Request.Context(), sandboxConfigTenantID(c), configID,
		)
		for _, row := range rows {
			if row == nil || row.Name == "" {
				continue
			}
			item := SkillInfoResponse{Name: row.Name, Description: row.Description}
			if index, ok := indexByName[row.Name]; ok {
				// Keep runtime behaviour consistent: an installed tenant Skill
				// shadows a preloaded Skill with the same name.
				response[index] = item
				continue
			}
			indexByName[row.Name] = len(response)
			response = append(response, item)
		}
	}

	if len(response) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success":          true,
			"data":             response,
			"skills_available": false,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":          true,
		"data":             response,
		"skills_available": true,
	})
}
