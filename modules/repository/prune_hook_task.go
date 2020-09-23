// Copyright 2020 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repository

import (
	"context"
	"fmt"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"

	"xorm.io/builder"
)

// PruneHookTaskTable deletes rows from hook_task as needed.
func PruneHookTaskTable(ctx context.Context) error {
	log.Trace("Doing: PruneHookTaskTable")

	if err := models.Iterate(
		models.DefaultDBContext(),
		new(models.Repository),
		builder.Gt{"ID": 0}.And(builder.Eq{"is_hook_task_purge_enabled": true}),
		func(idx int, bean interface{}) error {
			select {
			case <-ctx.Done():
				return fmt.Errorf("Aborted due to shutdown")
			default:
			}
			repo := bean.(*models.Repository)
			if repo.NumberWebhookDeliveriesToKeep < 0 {
				repo.NumberWebhookDeliveriesToKeep = setting.Repository.DefaultNumberWebhookDeliveriesToKeep
			}
			repoPath := repo.RepoPath()
			log.Trace("Running prune hook_task table on repository %s", repoPath)
			if err := models.DeleteDeliveredHookTasks(repo.ID, repo.NumberWebhookDeliveriesToKeep); err != nil {
				desc := fmt.Sprintf("Failed to prune hook_task on repository (%s): %v", repoPath, err)
				log.Warn(desc)
				if err = models.CreateRepositoryNotice(desc); err != nil {
					log.Error("CreateRepositoryNotice: %v", err)
				}
			}
			return nil
		},
	); err != nil {
		return err
	}

	log.Trace("Finished: PruneHookTaskTable")
	return nil
}
