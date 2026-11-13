// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Makes the repository's labels match .github/labels.yml, converted to JSON
// at LABELS_JSON. Called from .github/workflows/label-sync.yml through
// actions/github-script. With apply false it validates and logs only.

module.exports = async ({ github, context, core, apply }) => {
  const fs = require('fs');
  const wanted = JSON.parse(fs.readFileSync(process.env.LABELS_JSON, 'utf8'));

  // Validate the whole file before changing anything.
  const errors = [];
  const seen = new Set();
  if (!Array.isArray(wanted)) {
    errors.push('labels.yml must be a list of labels');
  }
  for (const [i, label] of (Array.isArray(wanted) ? wanted : []).entries()) {
    const where = `entry ${i + 1}${label?.name ? ` (${label.name})` : ''}`;
    if (typeof label?.name !== 'string' || label.name.trim() === '') {
      errors.push(`${where}: name must be a non-empty string`);
      continue;
    }
    const key = label.name.toLowerCase();
    if (seen.has(key)) {
      errors.push(`${where}: duplicate name; GitHub label names are case-insensitive`);
    }
    seen.add(key);
    if (typeof label.color !== 'string' || !/^[0-9a-fA-F]{6}$/.test(label.color)) {
      errors.push(`${where}: color must be six hex digits as a string, for example "ededed"`);
    }
    const description = label.description ?? '';
    if (typeof description !== 'string') {
      errors.push(`${where}: description must be a string`);
    } else if (description.length > 100) {
      errors.push(`${where}: description is ${description.length} characters; GitHub allows 100`);
    }
  }
  if (errors.length > 0) {
    errors.forEach((e) => core.error(e));
    core.setFailed(`labels.yml has ${errors.length} error(s)`);
    return;
  }

  const existing = await github.paginate(github.rest.issues.listLabelsForRepo, {
    ...context.repo,
    per_page: 100,
  });
  const byName = new Map(existing.map((l) => [l.name.toLowerCase(), l]));

  const tally = { created: 0, updated: 0, unchanged: 0 };
  for (const label of wanted) {
    const color = label.color.toLowerCase();
    const description = label.description ?? '';
    const current = byName.get(label.name.toLowerCase());
    if (!current) {
      core.info(`${apply ? 'create' : 'would create'}: ${label.name}`);
      if (apply) {
        await github.rest.issues.createLabel({ ...context.repo, name: label.name, color, description });
      }
      tally.created += 1;
    } else if (current.color.toLowerCase() !== color || (current.description ?? '') !== description) {
      core.info(`${apply ? 'update' : 'would update'}: ${label.name}`);
      if (apply) {
        // Address the label by its current name; the file's
        // spelling is not applied, so case-only renames stay manual.
        await github.rest.issues.updateLabel({ ...context.repo, name: current.name, color, description });
      }
      tally.updated += 1;
    } else {
      tally.unchanged += 1;
    }
  }

  const listed = new Set(wanted.map((l) => l.name.toLowerCase()));
  const unlisted = existing.filter((l) => !listed.has(l.name.toLowerCase())).map((l) => l.name);
  if (unlisted.length > 0) {
    core.notice(`Not in labels.yml, left alone: ${unlisted.join(', ')}`);
  }
  core.info(`${apply ? 'Applied' : 'Dry run'}: ${tally.created} created, ${tally.updated} updated, ${tally.unchanged} unchanged, ${unlisted.length} not listed`);
};
