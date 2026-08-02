#!/bin/sh

set -u

pattern='^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)\([a-z0-9][a-z0-9._/-]*\)!?: [a-z][^[:cntrl:]]*$'

check_subject() {
	subject=$1
	case "$subject" in
		Merge\ *)
			return 0
			;;
	esac
	if printf '%s\n' "$subject" | LC_ALL=C grep -Eq "$pattern"; then
		return 0
	fi
	printf 'Invalid commit subject: %s\n' "$subject" >&2
	printf 'Expected: type(scope): lowercase imperative summary\n' >&2
	return 1
}

if [ "$#" -eq 1 ] && [ -f "$1" ]; then
	IFS= read -r subject < "$1" || subject=
	check_subject "$subject"
	exit $?
fi

if [ "$#" -eq 2 ]; then
	base=$1
	head=$2
	commits=$(git log --format=%s "$base..$head")
	if [ -z "$commits" ]; then
		exit 0
	fi
	failed=0
	while IFS= read -r subject; do
		if ! check_subject "$subject"; then
			failed=1
		fi
	done <<EOF
$commits
EOF
	exit "$failed"
fi

printf 'usage: %s COMMIT_MSG_FILE | BASE HEAD\n' "$0" >&2
exit 2
