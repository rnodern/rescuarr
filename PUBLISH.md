# Publish Rescuarr on GitHub

Create an **empty** repository named `rescuarr` owned by the account `rnodern`:

https://github.com/new?name=rescuarr

Choose Public or Private as preferred. **Do not** initialise it with a README, licence or .gitignore, because this repository already has them.

In this directory, with GitHub CLI credentials or Git HTTPS/SSH access set up:

```bash
git remote add origin https://github.com/rnodern/rescuarr.git
git push -u origin main
```

If `origin` already exists, run `git remote set-url origin https://github.com/rnodern/rescuarr.git` instead of `git remote add`.

Once published, verify `https://github.com/rnodern/rescuarr` displays the README. A GitHub repo doesn't by itself give exclusive rights to the name.
