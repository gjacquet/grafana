package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"xorm.io/xorm"

	"github.com/grafana/grafana/pkg/infra/db"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/metrics"
	"github.com/grafana/grafana/pkg/models"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/dashboards"
	dashver "github.com/grafana/grafana/pkg/services/dashboardversion"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/quota"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrator"
	"github.com/grafana/grafana/pkg/services/sqlstore/permissions"
	"github.com/grafana/grafana/pkg/services/sqlstore/searchstore"
	"github.com/grafana/grafana/pkg/services/star"
	"github.com/grafana/grafana/pkg/services/store"
	"github.com/grafana/grafana/pkg/services/tag"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

type DashboardStore struct {
	store      db.DB
	cfg        *setting.Cfg
	log        log.Logger
	features   featuremgmt.FeatureToggles
	tagService tag.Service
}

type DashboardTag struct {
	Id          int64
	DashboardId int64
	Term        string
}

// DashboardStore implements the Store interface
var _ dashboards.Store = (*DashboardStore)(nil)

func ProvideDashboardStore(sqlStore db.DB, cfg *setting.Cfg, features featuremgmt.FeatureToggles, tagService tag.Service, quotaService quota.Service) (*DashboardStore, error) {
	s := &DashboardStore{store: sqlStore, cfg: cfg, log: log.New("dashboard-store"), features: features, tagService: tagService}

	defaultLimits, err := readQuotaConfig(cfg)
	if err != nil {
		return nil, err
	}

	if err := quotaService.RegisterQuotaReporter(&quota.NewUsageReporter{
		TargetSrv:     dashboards.QuotaTargetSrv,
		DefaultLimits: defaultLimits,
		Reporter:      s.Count,
	}); err != nil {
		return nil, err
	}

	return s, nil
}

func (d *DashboardStore) emitEntityEvent() bool {
	return d.features != nil && d.features.IsEnabled(featuremgmt.FlagPanelTitleSearch)
}

func (d *DashboardStore) ValidateDashboardBeforeSave(ctx context.Context, dashboard *models.Dashboard, overwrite bool) (bool, error) {
	isParentFolderChanged := false
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		var err error
		isParentFolderChanged, err = getExistingDashboardByIdOrUidForUpdate(sess, dashboard, d.store.GetDialect(), overwrite)
		if err != nil {
			return err
		}

		isParentFolderChanged, err = getExistingDashboardByTitleAndFolder(sess, dashboard, d.store.GetDialect(), overwrite,
			isParentFolderChanged)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	return isParentFolderChanged, nil
}

func (d *DashboardStore) GetFolderByTitle(ctx context.Context, orgID int64, title string) (*folder.Folder, error) {
	if title == "" {
		return nil, dashboards.ErrFolderTitleEmpty
	}

	// there is a unique constraint on org_id, folder_id, title
	// there are no nested folders so the parent folder id is always 0
	dashboard := models.Dashboard{OrgId: orgID, FolderId: 0, Title: title}
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		has, err := sess.Table(&models.Dashboard{}).Where("is_folder = " + d.store.GetDialect().BooleanStr(true)).Where("folder_id=0").Get(&dashboard)
		if err != nil {
			return err
		}
		if !has {
			return dashboards.ErrFolderNotFound
		}
		dashboard.SetId(dashboard.Id)
		dashboard.SetUid(dashboard.Uid)
		return nil
	})
	return folder.FromDashboard(&dashboard), err
}

func (d *DashboardStore) GetFolderByID(ctx context.Context, orgID int64, id int64) (*folder.Folder, error) {
	dashboard := models.Dashboard{OrgId: orgID, FolderId: 0, Id: id}
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		has, err := sess.Table(&models.Dashboard{}).Where("is_folder = " + d.store.GetDialect().BooleanStr(true)).Where("folder_id=0").Get(&dashboard)
		if err != nil {
			return err
		}
		if !has {
			return dashboards.ErrFolderNotFound
		}
		dashboard.SetId(dashboard.Id)
		dashboard.SetUid(dashboard.Uid)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return folder.FromDashboard(&dashboard), nil
}

func (d *DashboardStore) GetFolderByUID(ctx context.Context, orgID int64, uid string) (*folder.Folder, error) {
	if uid == "" {
		return nil, dashboards.ErrDashboardIdentifierNotSet
	}

	dashboard := models.Dashboard{OrgId: orgID, FolderId: 0, Uid: uid}
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		has, err := sess.Table(&models.Dashboard{}).Where("is_folder = " + d.store.GetDialect().BooleanStr(true)).Where("folder_id=0").Get(&dashboard)
		if err != nil {
			return err
		}
		if !has {
			return dashboards.ErrFolderNotFound
		}
		dashboard.SetId(dashboard.Id)
		dashboard.SetUid(dashboard.Uid)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return folder.FromDashboard(&dashboard), nil
}

func (d *DashboardStore) GetProvisionedDataByDashboardID(ctx context.Context, dashboardID int64) (*models.DashboardProvisioning, error) {
	var data models.DashboardProvisioning
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		_, err := sess.Where("dashboard_id = ?", dashboardID).Get(&data)
		return err
	})

	if data.DashboardId == 0 {
		return nil, nil
	}
	return &data, err
}

func (d *DashboardStore) GetProvisionedDataByDashboardUID(ctx context.Context, orgID int64, dashboardUID string) (*models.DashboardProvisioning, error) {
	var provisionedDashboard models.DashboardProvisioning
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		var dashboard models.Dashboard
		exists, err := sess.Where("org_id = ? AND uid = ?", orgID, dashboardUID).Get(&dashboard)
		if err != nil {
			return err
		}
		if !exists {
			return dashboards.ErrDashboardNotFound
		}
		exists, err = sess.Where("dashboard_id = ?", dashboard.Id).Get(&provisionedDashboard)
		if err != nil {
			return err
		}
		if !exists {
			return dashboards.ErrProvisionedDashboardNotFound
		}
		return nil
	})
	return &provisionedDashboard, err
}

func (d *DashboardStore) GetProvisionedDashboardData(ctx context.Context, name string) ([]*models.DashboardProvisioning, error) {
	var result []*models.DashboardProvisioning
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		return sess.Where("name = ?", name).Find(&result)
	})
	return result, err
}

func (d *DashboardStore) SaveProvisionedDashboard(ctx context.Context, cmd models.SaveDashboardCommand, provisioning *models.DashboardProvisioning) (*models.Dashboard, error) {
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		if err := saveDashboard(sess, &cmd, d.emitEntityEvent()); err != nil {
			return err
		}

		if provisioning.Updated == 0 {
			provisioning.Updated = cmd.Result.Updated.Unix()
		}

		return saveProvisionedData(sess, provisioning, cmd.Result)
	})

	return cmd.Result, err
}

func (d *DashboardStore) SaveDashboard(ctx context.Context, cmd models.SaveDashboardCommand) (*models.Dashboard, error) {
	err := d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		return saveDashboard(sess, &cmd, d.emitEntityEvent())
	})
	return cmd.Result, err
}

func (d *DashboardStore) UpdateDashboardACL(ctx context.Context, dashboardID int64, items []*models.DashboardACL) error {
	return d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		// delete existing items
		_, err := sess.Exec("DELETE FROM dashboard_acl WHERE dashboard_id=?", dashboardID)
		if err != nil {
			return fmt.Errorf("deleting from dashboard_acl failed: %w", err)
		}

		for _, item := range items {
			if item.UserID == 0 && item.TeamID == 0 && (item.Role == nil || !item.Role.IsValid()) {
				return models.ErrDashboardACLInfoMissing
			}

			if item.DashboardID == 0 {
				return models.ErrDashboardPermissionDashboardEmpty
			}

			sess.Nullable("user_id", "team_id")
			if _, err := sess.Insert(item); err != nil {
				return err
			}
		}

		// Update dashboard HasACL flag
		dashboard := models.Dashboard{HasACL: true}
		_, err = sess.Cols("has_acl").Where("id=?", dashboardID).Update(&dashboard)
		return err
	})
}

func (d *DashboardStore) SaveAlerts(ctx context.Context, dashID int64, alerts []*models.Alert) error {
	return d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		existingAlerts, err := GetAlertsByDashboardId2(dashID, sess)
		if err != nil {
			return err
		}

		if err := d.updateAlerts(ctx, existingAlerts, alerts, d.log); err != nil {
			return err
		}

		if err := d.deleteMissingAlerts(existingAlerts, alerts, sess); err != nil {
			return err
		}

		return nil
	})
}

// UnprovisionDashboard removes row in dashboard_provisioning for the dashboard making it seem as if manually created.
// The dashboard will still have `created_by = -1` to see it was not created by any particular user.
func (d *DashboardStore) UnprovisionDashboard(ctx context.Context, id int64) error {
	return d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		_, err := sess.Where("dashboard_id = ?", id).Delete(&models.DashboardProvisioning{})
		return err
	})
}

func (d *DashboardStore) DeleteOrphanedProvisionedDashboards(ctx context.Context, cmd *models.DeleteOrphanedProvisionedDashboardsCommand) error {
	return d.store.WithDbSession(ctx, func(sess *db.Session) error {
		var result []*models.DashboardProvisioning

		convertedReaderNames := make([]interface{}, len(cmd.ReaderNames))
		for index, readerName := range cmd.ReaderNames {
			convertedReaderNames[index] = readerName
		}

		err := sess.NotIn("name", convertedReaderNames...).Find(&result)
		if err != nil {
			return err
		}

		for _, deleteDashCommand := range result {
			err := d.DeleteDashboard(ctx, &models.DeleteDashboardCommand{Id: deleteDashCommand.DashboardId})
			if err != nil && !errors.Is(err, dashboards.ErrDashboardNotFound) {
				return err
			}
		}

		return nil
	})
}

func (d *DashboardStore) Count(ctx context.Context, scopeParams *quota.ScopeParameters) (*quota.Map, error) {
	u := &quota.Map{}
	type result struct {
		Count int64
	}

	r := result{}
	if err := d.store.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		rawSQL := fmt.Sprintf("SELECT COUNT(*) AS count FROM dashboard WHERE is_folder=%s", d.store.GetDialect().BooleanStr(false))
		if _, err := sess.SQL(rawSQL).Get(&r); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return u, err
	} else {
		tag, err := quota.NewTag(dashboards.QuotaTargetSrv, dashboards.QuotaTarget, quota.GlobalScope)
		if err != nil {
			return nil, err
		}
		u.Set(tag, r.Count)
	}

	if scopeParams.OrgID != 0 {
		if err := d.store.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
			rawSQL := fmt.Sprintf("SELECT COUNT(*) AS count FROM dashboard WHERE org_id=? AND is_folder=%s", d.store.GetDialect().BooleanStr(false))
			if _, err := sess.SQL(rawSQL, scopeParams.OrgID).Get(&r); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return u, err
		} else {
			tag, err := quota.NewTag(dashboards.QuotaTargetSrv, dashboards.QuotaTarget, quota.OrgScope)
			if err != nil {
				return nil, err
			}
			u.Set(tag, r.Count)
		}
	}

	return u, nil
}

func getExistingDashboardByIdOrUidForUpdate(sess *db.Session, dash *models.Dashboard, dialect migrator.Dialect, overwrite bool) (bool, error) {
	dashWithIdExists := false
	isParentFolderChanged := false
	var existingById models.Dashboard

	if dash.Id > 0 {
		var err error
		dashWithIdExists, err = sess.Where("id=? AND org_id=?", dash.Id, dash.OrgId).Get(&existingById)
		if err != nil {
			return false, fmt.Errorf("SQL query for existing dashboard by ID failed: %w", err)
		}

		if !dashWithIdExists {
			return false, dashboards.ErrDashboardNotFound
		}

		if dash.Uid == "" {
			dash.SetUid(existingById.Uid)
		}
	}

	dashWithUidExists := false
	var existingByUid models.Dashboard

	if dash.Uid != "" {
		var err error
		dashWithUidExists, err = sess.Where("org_id=? AND uid=?", dash.OrgId, dash.Uid).Get(&existingByUid)
		if err != nil {
			return false, fmt.Errorf("SQL query for existing dashboard by UID failed: %w", err)
		}
	}

	if dash.FolderId > 0 {
		var existingFolder models.Dashboard
		folderExists, err := sess.Where("org_id=? AND id=? AND is_folder=?", dash.OrgId, dash.FolderId,
			dialect.BooleanStr(true)).Get(&existingFolder)
		if err != nil {
			return false, fmt.Errorf("SQL query for folder failed: %w", err)
		}

		if !folderExists {
			return false, dashboards.ErrDashboardFolderNotFound
		}
	}

	if !dashWithIdExists && !dashWithUidExists {
		return false, nil
	}

	if dashWithIdExists && dashWithUidExists && existingById.Id != existingByUid.Id {
		return false, dashboards.ErrDashboardWithSameUIDExists
	}

	existing := existingById

	if !dashWithIdExists && dashWithUidExists {
		dash.SetId(existingByUid.Id)
		dash.SetUid(existingByUid.Uid)
		existing = existingByUid
	}

	if (existing.IsFolder && !dash.IsFolder) ||
		(!existing.IsFolder && dash.IsFolder) {
		return isParentFolderChanged, dashboards.ErrDashboardTypeMismatch
	}

	if !dash.IsFolder && dash.FolderId != existing.FolderId {
		isParentFolderChanged = true
	}

	// check for is someone else has written in between
	if dash.Version != existing.Version {
		if overwrite {
			dash.SetVersion(existing.Version)
		} else {
			return isParentFolderChanged, dashboards.ErrDashboardVersionMismatch
		}
	}

	// do not allow plugin dashboard updates without overwrite flag
	if existing.PluginId != "" && !overwrite {
		return isParentFolderChanged, dashboards.UpdatePluginDashboardError{PluginId: existing.PluginId}
	}

	return isParentFolderChanged, nil
}

func getExistingDashboardByTitleAndFolder(sess *db.Session, dash *models.Dashboard, dialect migrator.Dialect, overwrite,
	isParentFolderChanged bool) (bool, error) {
	var existing models.Dashboard
	exists, err := sess.Where("org_id=? AND slug=? AND (is_folder=? OR folder_id=?)", dash.OrgId, dash.Slug,
		dialect.BooleanStr(true), dash.FolderId).Get(&existing)
	if err != nil {
		return isParentFolderChanged, fmt.Errorf("SQL query for existing dashboard by org ID or folder ID failed: %w", err)
	}

	if exists && dash.Id != existing.Id {
		if existing.IsFolder && !dash.IsFolder {
			return isParentFolderChanged, dashboards.ErrDashboardWithSameNameAsFolder
		}

		if !existing.IsFolder && dash.IsFolder {
			return isParentFolderChanged, dashboards.ErrDashboardFolderWithSameNameAsDashboard
		}

		if !dash.IsFolder && (dash.FolderId != existing.FolderId || dash.Id == 0) {
			isParentFolderChanged = true
		}

		if overwrite {
			dash.SetId(existing.Id)
			dash.SetUid(existing.Uid)
			dash.SetVersion(existing.Version)
		} else {
			return isParentFolderChanged, dashboards.ErrDashboardWithSameNameInFolderExists
		}
	}

	return isParentFolderChanged, nil
}

func saveDashboard(sess *db.Session, cmd *models.SaveDashboardCommand, emitEntityEvent bool) error {
	dash := cmd.GetDashboardModel()

	userId := cmd.UserId

	if userId == 0 {
		userId = -1
	}

	if dash.Id > 0 {
		var existing models.Dashboard
		dashWithIdExists, err := sess.Where("id=? AND org_id=?", dash.Id, dash.OrgId).Get(&existing)
		if err != nil {
			return err
		}
		if !dashWithIdExists {
			return dashboards.ErrDashboardNotFound
		}

		// check for is someone else has written in between
		if dash.Version != existing.Version {
			if cmd.Overwrite {
				dash.SetVersion(existing.Version)
			} else {
				return dashboards.ErrDashboardVersionMismatch
			}
		}

		// do not allow plugin dashboard updates without overwrite flag
		if existing.PluginId != "" && !cmd.Overwrite {
			return dashboards.UpdatePluginDashboardError{PluginId: existing.PluginId}
		}
	}

	if dash.Uid == "" {
		uid, err := generateNewDashboardUid(sess, dash.OrgId)
		if err != nil {
			return err
		}
		dash.SetUid(uid)
	}

	parentVersion := dash.Version
	var affectedRows int64
	var err error

	if dash.Id == 0 {
		dash.SetVersion(1)
		dash.Created = time.Now()
		dash.CreatedBy = userId
		dash.Updated = time.Now()
		dash.UpdatedBy = userId
		metrics.MApiDashboardInsert.Inc()
		affectedRows, err = sess.Insert(dash)
	} else {
		dash.SetVersion(dash.Version + 1)

		if !cmd.UpdatedAt.IsZero() {
			dash.Updated = cmd.UpdatedAt
		} else {
			dash.Updated = time.Now()
		}

		dash.UpdatedBy = userId

		affectedRows, err = sess.MustCols("folder_id").ID(dash.Id).Update(dash)
	}

	if err != nil {
		return err
	}

	if affectedRows == 0 {
		return dashboards.ErrDashboardNotFound
	}

	dashVersion := &dashver.DashboardVersion{
		DashboardID:   dash.Id,
		ParentVersion: parentVersion,
		RestoredFrom:  cmd.RestoredFrom,
		Version:       dash.Version,
		Created:       time.Now(),
		CreatedBy:     dash.UpdatedBy,
		Message:       cmd.Message,
		Data:          dash.Data,
	}

	// insert version entry
	if affectedRows, err = sess.Insert(dashVersion); err != nil {
		return err
	} else if affectedRows == 0 {
		return dashboards.ErrDashboardNotFound
	}

	// delete existing tags
	if _, err = sess.Exec("DELETE FROM dashboard_tag WHERE dashboard_id=?", dash.Id); err != nil {
		return err
	}

	// insert new tags
	tags := dash.GetTags()
	if len(tags) > 0 {
		for _, tag := range tags {
			if _, err := sess.Insert(DashboardTag{DashboardId: dash.Id, Term: tag}); err != nil {
				return err
			}
		}
	}

	cmd.Result = dash

	if emitEntityEvent {
		_, err := sess.Insert(createEntityEvent(dash, store.EntityEventTypeUpdate))
		if err != nil {
			return err
		}
	}
	return nil
}

func generateNewDashboardUid(sess *db.Session, orgId int64) (string, error) {
	for i := 0; i < 3; i++ {
		uid := util.GenerateShortUID()

		exists, err := sess.Where("org_id=? AND uid=?", orgId, uid).Get(&models.Dashboard{})
		if err != nil {
			return "", err
		}

		if !exists {
			return uid, nil
		}
	}

	return "", dashboards.ErrDashboardFailedGenerateUniqueUid
}

func saveProvisionedData(sess *db.Session, provisioning *models.DashboardProvisioning, dashboard *models.Dashboard) error {
	result := &models.DashboardProvisioning{}

	exist, err := sess.Where("dashboard_id=? AND name = ?", dashboard.Id, provisioning.Name).Get(result)
	if err != nil {
		return err
	}

	provisioning.Id = result.Id
	provisioning.DashboardId = dashboard.Id

	if exist {
		_, err = sess.ID(result.Id).Update(provisioning)
	} else {
		_, err = sess.Insert(provisioning)
	}

	return err
}

func GetAlertsByDashboardId2(dashboardId int64, sess *db.Session) ([]*models.Alert, error) {
	alerts := make([]*models.Alert, 0)
	err := sess.Where("dashboard_id = ?", dashboardId).Find(&alerts)

	if err != nil {
		return []*models.Alert{}, err
	}

	return alerts, nil
}

func (d *DashboardStore) updateAlerts(ctx context.Context, existingAlerts []*models.Alert, alerts []*models.Alert, log log.Logger) error {
	return d.store.WithDbSession(ctx, func(sess *db.Session) error {
		for _, alert := range alerts {
			update := false
			var alertToUpdate *models.Alert

			for _, k := range existingAlerts {
				if alert.PanelId == k.PanelId {
					update = true
					alert.Id = k.Id
					alertToUpdate = k
					break
				}
			}

			if update {
				if alertToUpdate.ContainsUpdates(alert) {
					alert.Updated = time.Now()
					alert.State = alertToUpdate.State
					sess.MustCols("message", "for")

					_, err := sess.ID(alert.Id).Update(alert)
					if err != nil {
						return err
					}

					log.Debug("Alert updated", "name", alert.Name, "id", alert.Id)
				}
			} else {
				alert.Updated = time.Now()
				alert.Created = time.Now()
				alert.State = models.AlertStateUnknown
				alert.NewStateDate = time.Now()

				_, err := sess.Insert(alert)
				if err != nil {
					return err
				}

				log.Debug("Alert inserted", "name", alert.Name, "id", alert.Id)
			}
			tags := alert.GetTagsFromSettings()
			if _, err := sess.Exec("DELETE FROM alert_rule_tag WHERE alert_id = ?", alert.Id); err != nil {
				return err
			}
			if tags != nil {
				tags, err := d.tagService.EnsureTagsExist(ctx, tags)
				if err != nil {
					return err
				}
				for _, tag := range tags {
					if _, err := sess.Exec("INSERT INTO alert_rule_tag (alert_id, tag_id) VALUES(?,?)", alert.Id, tag.Id); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func (d *DashboardStore) deleteMissingAlerts(alerts []*models.Alert, existingAlerts []*models.Alert, sess *db.Session) error {
	for _, missingAlert := range alerts {
		missing := true

		for _, k := range existingAlerts {
			if missingAlert.PanelId == k.PanelId {
				missing = false
				break
			}
		}

		if missing {
			if err := d.deleteAlertByIdInternal(missingAlert.Id, "Removed from dashboard", sess); err != nil {
				// No use trying to delete more, since we're in a transaction and it will be
				// rolled back on error.
				return err
			}
		}
	}

	return nil
}

func (d *DashboardStore) deleteAlertByIdInternal(alertId int64, reason string, sess *db.Session) error {
	d.log.Debug("Deleting alert", "id", alertId, "reason", reason)

	if _, err := sess.Exec("DELETE FROM alert WHERE id = ?", alertId); err != nil {
		return err
	}

	if _, err := sess.Exec("DELETE FROM annotation WHERE alert_id = ?", alertId); err != nil {
		return err
	}

	if _, err := sess.Exec("DELETE FROM alert_notification_state WHERE alert_id = ?", alertId); err != nil {
		return err
	}

	if _, err := sess.Exec("DELETE FROM alert_rule_tag WHERE alert_id = ?", alertId); err != nil {
		return err
	}

	return nil
}

func (d *DashboardStore) GetDashboardsByPluginID(ctx context.Context, query *models.GetDashboardsByPluginIdQuery) error {
	return d.store.WithDbSession(ctx, func(dbSession *db.Session) error {
		var dashboards = make([]*models.Dashboard, 0)
		whereExpr := "org_id=? AND plugin_id=? AND is_folder=" + d.store.GetDialect().BooleanStr(false)

		err := dbSession.Where(whereExpr, query.OrgId, query.PluginId).Find(&dashboards)
		query.Result = dashboards
		return err
	})
}

func (d *DashboardStore) DeleteDashboard(ctx context.Context, cmd *models.DeleteDashboardCommand) error {
	return d.store.WithTransactionalDbSession(ctx, func(sess *db.Session) error {
		return d.deleteDashboard(cmd, sess, d.emitEntityEvent())
	})
}

func (d *DashboardStore) deleteDashboard(cmd *models.DeleteDashboardCommand, sess *db.Session, emitEntityEvent bool) error {
	dashboard := models.Dashboard{Id: cmd.Id, OrgId: cmd.OrgId}
	has, err := sess.Get(&dashboard)
	if err != nil {
		return err
	} else if !has {
		return dashboards.ErrDashboardNotFound
	}

	deletes := []string{
		"DELETE FROM dashboard_tag WHERE dashboard_id = ? ",
		"DELETE FROM star WHERE dashboard_id = ? ",
		"DELETE FROM dashboard_public WHERE dashboard_uid = (SELECT uid FROM dashboard WHERE id = ?)",
		"DELETE FROM dashboard WHERE id = ?",
		"DELETE FROM playlist_item WHERE type = 'dashboard_by_id' AND value = ?",
		"DELETE FROM dashboard_version WHERE dashboard_id = ?",
		"DELETE FROM annotation WHERE dashboard_id = ?",
		"DELETE FROM dashboard_provisioning WHERE dashboard_id = ?",
		"DELETE FROM dashboard_acl WHERE dashboard_id = ?",
	}

	if dashboard.IsFolder {
		deletes = append(deletes, "DELETE FROM dashboard WHERE folder_id = ?")

		var dashIds []struct {
			Id  int64
			Uid string
		}
		err := sess.SQL("SELECT id, uid FROM dashboard WHERE folder_id = ?", dashboard.Id).Find(&dashIds)
		if err != nil {
			return err
		}

		for _, id := range dashIds {
			if err := d.deleteAlertDefinition(id.Id, sess); err != nil {
				return err
			}
		}

		// remove all access control permission with folder scope
		_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", dashboards.ScopeFoldersProvider.GetResourceScopeUID(dashboard.Uid))
		if err != nil {
			return err
		}

		for _, dash := range dashIds {
			// remove all access control permission with child dashboard scopes
			_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", ac.GetResourceScopeUID("dashboards", dash.Uid))
			if err != nil {
				return err
			}
		}

		if len(dashIds) > 0 {
			childrenDeletes := []string{
				"DELETE FROM dashboard_tag WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM star WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_version WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM annotation WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_provisioning WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_acl WHERE dashboard_id IN (SELECT id FROM dashboard WHERE org_id = ? AND folder_id = ?)",
				"DELETE FROM dashboard_public WHERE dashboard_uid IN (SELECT uid FROM dashboard WHERE org_id = ? AND folder_id = ?)",
			}
			for _, sql := range childrenDeletes {
				_, err := sess.Exec(sql, dashboard.OrgId, dashboard.Id)
				if err != nil {
					return err
				}
			}
		}

		var existingRuleID int64
		exists, err := sess.Table("alert_rule").Where("namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)", dashboard.Id).Cols("id").Get(&existingRuleID)
		if err != nil {
			return err
		}
		if exists {
			if !cmd.ForceDeleteFolderRules {
				return fmt.Errorf("folder cannot be deleted: %w", dashboards.ErrFolderContainsAlertRules)
			}

			// Delete all rules under this folder.
			deleteNGAlertsByFolder := []string{
				"DELETE FROM alert_rule WHERE namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)",
				"DELETE FROM alert_rule_version WHERE rule_namespace_uid = (SELECT uid FROM dashboard WHERE id = ?)",
			}

			for _, sql := range deleteNGAlertsByFolder {
				_, err := sess.Exec(sql, dashboard.Id)
				if err != nil {
					return err
				}
			}
		}
	} else {
		_, err = sess.Exec("DELETE FROM permission WHERE scope = ?", ac.GetResourceScopeUID("dashboards", dashboard.Uid))
		if err != nil {
			return err
		}
	}

	if err := d.deleteAlertDefinition(dashboard.Id, sess); err != nil {
		return err
	}

	for _, sql := range deletes {
		_, err := sess.Exec(sql, dashboard.Id)
		if err != nil {
			return err
		}
	}

	if emitEntityEvent {
		_, err := sess.Insert(createEntityEvent(&dashboard, store.EntityEventTypeDelete))
		if err != nil {
			return err
		}
	}
	return nil
}

func createEntityEvent(dashboard *models.Dashboard, eventType store.EntityEventType) *store.EntityEvent {
	var entityEvent *store.EntityEvent
	if dashboard.IsFolder {
		entityEvent = &store.EntityEvent{
			EventType: eventType,
			EntityId:  store.CreateDatabaseEntityId(dashboard.Uid, dashboard.OrgId, store.EntityTypeFolder),
			Created:   time.Now().Unix(),
		}
	} else {
		entityEvent = &store.EntityEvent{
			EventType: eventType,
			EntityId:  store.CreateDatabaseEntityId(dashboard.Uid, dashboard.OrgId, store.EntityTypeDashboard),
			Created:   time.Now().Unix(),
		}
	}
	return entityEvent
}

func (d *DashboardStore) deleteAlertDefinition(dashboardId int64, sess *db.Session) error {
	alerts := make([]*models.Alert, 0)
	if err := sess.Where("dashboard_id = ?", dashboardId).Find(&alerts); err != nil {
		return err
	}

	for _, alert := range alerts {
		if err := d.deleteAlertByIdInternal(alert.Id, "Dashboard deleted", sess); err != nil {
			// If we return an error, the current transaction gets rolled back, so no use
			// trying to delete more
			return err
		}
	}

	return nil
}

func (d *DashboardStore) GetDashboard(ctx context.Context, query *models.GetDashboardQuery) (*models.Dashboard, error) {
	err := d.store.WithDbSession(ctx, func(sess *db.Session) error {
		if query.Id == 0 && len(query.Slug) == 0 && len(query.Uid) == 0 {
			return dashboards.ErrDashboardIdentifierNotSet
		}

		dashboard := models.Dashboard{Slug: query.Slug, OrgId: query.OrgId, Id: query.Id, Uid: query.Uid}
		has, err := sess.Get(&dashboard)

		if err != nil {
			return err
		} else if !has {
			return dashboards.ErrDashboardNotFound
		}

		dashboard.SetId(dashboard.Id)
		dashboard.SetUid(dashboard.Uid)
		query.Result = &dashboard
		return nil
	})

	return query.Result, err
}

func (d *DashboardStore) GetDashboardUIDById(ctx context.Context, query *models.GetDashboardRefByIdQuery) error {
	return d.store.WithDbSession(ctx, func(sess *db.Session) error {
		var rawSQL = `SELECT uid, slug from dashboard WHERE Id=?`
		us := &models.DashboardRef{}
		exists, err := sess.SQL(rawSQL, query.Id).Get(us)
		if err != nil {
			return err
		} else if !exists {
			return dashboards.ErrDashboardNotFound
		}
		query.Result = us
		return nil
	})
}

func (d *DashboardStore) GetDashboards(ctx context.Context, query *models.GetDashboardsQuery) error {
	return d.store.WithDbSession(ctx, func(sess *db.Session) error {
		if len(query.DashboardIds) == 0 && len(query.DashboardUIds) == 0 {
			return star.ErrCommandValidationFailed
		}

		var dashboards = make([]*models.Dashboard, 0)
		var session *xorm.Session
		if len(query.DashboardIds) > 0 {
			session = sess.In("id", query.DashboardIds)
		} else {
			session = sess.In("uid", query.DashboardUIds)
		}

		err := session.Find(&dashboards)
		query.Result = dashboards
		return err
	})
}

func (d *DashboardStore) FindDashboards(ctx context.Context, query *models.FindPersistedDashboardsQuery) ([]dashboards.DashboardSearchProjection, error) {
	filters := []interface{}{
		permissions.DashboardPermissionFilter{
			OrgRole:         query.SignedInUser.OrgRole,
			OrgId:           query.SignedInUser.OrgID,
			Dialect:         d.store.GetDialect(),
			UserId:          query.SignedInUser.UserID,
			PermissionLevel: query.Permission,
		},
	}

	if !ac.IsDisabled(d.cfg) {
		// if access control is enabled, overwrite the filters so far
		filters = []interface{}{
			permissions.NewAccessControlDashboardPermissionFilter(query.SignedInUser, query.Permission, query.Type),
		}
	}

	for _, filter := range query.Sort.Filter {
		filters = append(filters, filter)
	}

	filters = append(filters, query.Filters...)

	if query.OrgId != 0 {
		filters = append(filters, searchstore.OrgFilter{OrgId: query.OrgId})
	} else if query.SignedInUser.OrgID != 0 {
		filters = append(filters, searchstore.OrgFilter{OrgId: query.SignedInUser.OrgID})
	}

	if len(query.Tags) > 0 {
		filters = append(filters, searchstore.TagsFilter{Tags: query.Tags})
	}

	if len(query.DashboardUIDs) > 0 {
		filters = append(filters, searchstore.DashboardFilter{UIDs: query.DashboardUIDs})
	} else if len(query.DashboardIds) > 0 {
		filters = append(filters, searchstore.DashboardIDFilter{IDs: query.DashboardIds})
	}

	if query.IsStarred {
		filters = append(filters, searchstore.StarredFilter{UserId: query.SignedInUser.UserID})
	}

	if len(query.Title) > 0 {
		filters = append(filters, searchstore.TitleFilter{Dialect: d.store.GetDialect(), Title: query.Title})
	}

	if len(query.Type) > 0 {
		filters = append(filters, searchstore.TypeFilter{Dialect: d.store.GetDialect(), Type: query.Type})
	}

	if len(query.FolderIds) > 0 {
		filters = append(filters, searchstore.FolderFilter{IDs: query.FolderIds})
	}

	var res []dashboards.DashboardSearchProjection
	sb := &searchstore.Builder{Dialect: d.store.GetDialect(), Filters: filters}

	limit := query.Limit
	if limit < 1 {
		limit = 1000
	}

	page := query.Page
	if page < 1 {
		page = 1
	}

	sql, params := sb.ToSQL(limit, page)

	err := d.store.WithDbSession(ctx, func(sess *db.Session) error {
		return sess.SQL(sql, params...).Find(&res)
	})

	if err != nil {
		return nil, err
	}

	return res, nil
}

func (d *DashboardStore) GetDashboardTags(ctx context.Context, query *models.GetDashboardTagsQuery) error {
	return d.store.WithDbSession(ctx, func(dbSession *db.Session) error {
		sql := `SELECT
					  COUNT(*) as count,
						term
					FROM dashboard
					INNER JOIN dashboard_tag on dashboard_tag.dashboard_id = dashboard.id
					WHERE dashboard.org_id=?
					GROUP BY term
					ORDER BY term`

		query.Result = make([]*models.DashboardTagCloudItem, 0)
		sess := dbSession.SQL(sql, query.OrgId)
		err := sess.Find(&query.Result)
		return err
	})
}

// CountDashboardsInFolder returns a count of all dashboards associated with the
// given parent folder ID.
//
// This will be updated to take CountDashboardsInFolderQuery as an argument and
// lookup dashboards using the ParentFolderUID when dashboards are associated with a parent folder UID instead of ID.
func (d *DashboardStore) CountDashboardsInFolder(
	ctx context.Context, req *dashboards.CountDashboardsInFolderRequest) (int64, error) {
	var count int64
	var err error
	err = d.store.WithDbSession(ctx, func(sess *db.Session) error {
		session := sess.In("folder_id", req.FolderID).In("org_id", req.OrgID).
			In("is_folder", d.store.GetDialect().BooleanStr(false))
		count, err = session.Count(&models.Dashboard{})
		return err
	})
	return count, err
}

func readQuotaConfig(cfg *setting.Cfg) (*quota.Map, error) {
	limits := &quota.Map{}

	if cfg == nil {
		return limits, nil
	}

	globalQuotaTag, err := quota.NewTag(dashboards.QuotaTargetSrv, dashboards.QuotaTarget, quota.GlobalScope)
	if err != nil {
		return &quota.Map{}, err
	}
	orgQuotaTag, err := quota.NewTag(dashboards.QuotaTargetSrv, dashboards.QuotaTarget, quota.OrgScope)
	if err != nil {
		return &quota.Map{}, err
	}

	limits.Set(globalQuotaTag, cfg.Quota.Global.Dashboard)
	limits.Set(orgQuotaTag, cfg.Quota.Org.Dashboard)
	return limits, nil
}
