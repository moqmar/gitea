// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"context"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/structs"
)

// UpdateMigrationPosterIDAll updates all migrated repositories' issues and comments posterID
// **for Cron Job only**
// 1. Itterate throu all repos who are migrated from structs.SupportedFullGitService   ->>>> use UpdateMigrationPosterIDByRepo(repo)
// -> check if there is a configured Oauth2 source who match base url
//  -> or each repo-content (issue/pull/reactions/ ...)
//    -> QueryMatch Oauth2-user-id and PosterID  of repo -> matches get replaced by matching user of Oauth2-User account
func UpdateMigrationPosterIDAll(ctx context.Context) error {
	for _, gitService := range structs.SupportedFullGitService {
		select {
		case <-ctx.Done():
			log.Warn("UpdateMigrationPosterIDAll aborted before %s", gitService.Name())
			return models.ErrCancelledf("during UpdateMigrationPosterIDAll before %s", gitService.Name())
		default:
		}
		if err := updateMigrationPosterIDByGitService(ctx, gitService); err != nil {
			log.Error("updateMigrationPosterIDByGitService failed: %v", err)
		}
	}
	return nil
}

// ToDo UpdateMigrationPosterIDByRepo(repo)
// **for a sepcific repo**
// -> check if there is a configured Oauth2 source who match base url
//  -> or each repo-content (issue/pull/reactions/ ...)
//    -> QueryMatch Oauth2-user-id and PosterID  of repo -> matches get replaced by matching user of Oauth2-User account

// ToDo UpdateMigrationPosterIDByOauth2User(user)
// **get exec after new user with Oauth2 was created or Oauth2 was linked to a user**
// -> get all repos who have same base url as Oauth2 source
//  -> or each repo-content (issue/pull/reactions/ ...)
//    -> QueryMatch Oauth2-user-id and PosterID  of repo -> matches get replaced by matching user of Oauth2-User account

func updateMigrationPosterIDByGitService(ctx context.Context, tp structs.GitServiceType) error {
	provider := tp.Name()
	if len(provider) == 0 {
		return nil
	}

	const batchSize = 100
	var start int
	for {
		select {
		case <-ctx.Done():
			log.Warn("UpdateMigrationPosterIDByGitService(%s) cancelled", tp.Name())
			return nil
		default:
		}

		users, err := models.FindExternalUsersByProvider(models.FindExternalUserOptions{
			Provider: provider,
			Start:    start,
			Limit:    batchSize,
		})
		if err != nil {
			return err
		}

		for _, user := range users {
			select {
			case <-ctx.Done():
				log.Warn("UpdateMigrationPosterIDByGitService(%s) cancelled", tp.Name())
				return nil
			default:
			}
			externalUserID := user.ExternalID
			if err := models.UpdateMigrationsByType(tp, externalUserID, user.UserID); err != nil {
				log.Error("UpdateMigrationsByType type %s external user id %v to local user id %v failed: %v", tp.Name(), user.ExternalID, user.UserID, err)
			}
		}

		if len(users) < batchSize {
			break
		}
		start += len(users)
	}
	return nil
}
