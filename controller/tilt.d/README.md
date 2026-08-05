# `tilt.d`

Put personal Tilt extensions in this directory as `*.tilt` or `*.star` files.

The main [`Tiltfile`](../Tiltfile) includes those files after the built-in CWE resources and buttons are defined. Use this for local-only additions such as:

- extra `local_resource(...)` helpers
- extra `cmd_button(...)` actions
- extra port-forwards or links

This directory is gitignored except for this README.
