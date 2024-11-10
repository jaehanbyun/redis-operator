#!/bin/bash

# 이전 모든 커밋에 대해 Sign-off를 추가하고 커밋 시간 유지
while true; do
    COMMIT_DATE=$(git show -s --format=%ci HEAD)

    GIT_COMMITTER_DATE="$COMMIT_DATE" git commit --amend --no-edit -s --date="$COMMIT_DATE"

    if ! git rebase --continue; then
        break
    fi
done
