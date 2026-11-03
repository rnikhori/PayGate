#!/usr/bin/env python3
"""
Bulk-create labels, milestones, and issues on a GitHub repo for the PayGate backlog.

Usage:
    python3 scripts/create_github_issues.py --repo <owner>/<name> [--dry-run]

Reads issues/labels.json, issues/milestones.json, issues/issues.json (relative to repo root).
Resumable: records created issue titles in issues/.created_issues.log and skips them on rerun,
so an interrupted run (rate limit, network blip) can just be re-invoked.

Requires: `gh` CLI authenticated (gh auth status) with `repo` scope.
"""
import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ISSUES_DIR = ROOT / "issues"
PROGRESS_LOG = ISSUES_DIR / ".created_issues.log"
SLEEP_BETWEEN_CALLS = 1.2  # seconds, stay well under secondary rate limits


def run(cmd, dry_run):
    if dry_run:
        print("DRY RUN:", " ".join(cmd))
        return ""
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"Command failed: {' '.join(cmd)}\n{result.stderr}")
    return result.stdout.strip()


def load_json(name):
    with open(ISSUES_DIR / name) as f:
        return json.load(f)


def ensure_labels(repo, labels, dry_run):
    existing = set()
    if not dry_run:
        out = run(["gh", "label", "list", "--repo", repo, "--limit", "300", "--json", "name"], dry_run)
        existing = {l["name"] for l in json.loads(out)}
    for label in labels:
        if label["name"] in existing:
            continue
        print(f"Creating label: {label['name']}")
        run(
            [
                "gh", "label", "create", label["name"],
                "--repo", repo,
                "--color", label["color"],
                "--description", label.get("description", ""),
                "--force",
            ],
            dry_run,
        )
        time.sleep(0.3)


def ensure_milestones(repo, milestones, dry_run):
    existing = {}
    if not dry_run:
        out = run(["gh", "api", f"repos/{repo}/milestones", "-f", "state=all", "--paginate"], dry_run)
        for m in json.loads(out):
            existing[m["title"]] = m["number"]
    for m in milestones:
        if m["title"] in existing:
            continue
        print(f"Creating milestone: {m['title']}")
        run(
            [
                "gh", "api", f"repos/{repo}/milestones", "-X", "POST",
                "-f", f"title={m['title']}",
                "-f", f"description={m['description']}",
            ],
            dry_run,
        )
        time.sleep(0.3)


def load_progress():
    if PROGRESS_LOG.exists():
        return set(PROGRESS_LOG.read_text().splitlines())
    return set()


def mark_done(title):
    with open(PROGRESS_LOG, "a") as f:
        f.write(title + "\n")


def create_issues(repo, issues, dry_run):
    done = load_progress()
    total = len(issues)
    for i, issue in enumerate(issues, 1):
        if issue["title"] in done:
            print(f"[{i}/{total}] skip (already created): {issue['title']}")
            continue
        cmd = [
            "gh", "issue", "create",
            "--repo", repo,
            "--title", issue["title"],
            "--body", issue["body"],
            "--milestone", issue["milestone"],
        ]
        for label in issue["labels"]:
            cmd += ["--label", label]
        print(f"[{i}/{total}] creating: {issue['title']}")
        run(cmd, dry_run)
        if not dry_run:
            mark_done(issue["title"])
        time.sleep(SLEEP_BETWEEN_CALLS)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", required=True, help="owner/name, e.g. rnikhori/PayGate")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    labels = load_json("labels.json")
    milestones = load_json("milestones.json")
    issues = load_json("issues.json")

    print(f"Repo: {args.repo}")
    print(f"Labels: {len(labels)}  Milestones: {len(milestones)}  Issues: {len(issues)}")
    if args.dry_run:
        print("(dry run — no changes will be made)")

    ensure_labels(args.repo, labels, args.dry_run)
    ensure_milestones(args.repo, milestones, args.dry_run)
    create_issues(args.repo, issues, args.dry_run)

    print("Done.")


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as e:
        print(f"ERROR: {e}", file=sys.stderr)
        sys.exit(1)
