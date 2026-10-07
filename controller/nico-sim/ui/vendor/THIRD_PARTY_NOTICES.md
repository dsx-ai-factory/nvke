# Third-party notices: nico-sim UI

The nico-sim UI loads the libraries below from this directory. They are
unmodified copies of upstream releases, distributed through
[jsDelivr](https://www.jsdelivr.com/), and carry no NVIDIA copyright.
Versions come from the banner in each file, except the Nord theme, which has none.

| Component | Version | Files | Origin | License |
|---|---|---|---|---|
| xterm.js (`@xterm/xterm`) | 5.5.0 | `xterm/xterm.min.js`, `xterm/xterm.min.css` | https://github.com/xtermjs/xterm.js | MIT |
| xterm.js fit addon (`@xterm/addon-fit`) | 0.10.0 | `xterm/addon-fit.min.js` | https://github.com/xtermjs/xterm.js | MIT |
| xterm.js search addon (`@xterm/addon-search`) | 0.15.0 | `xterm/addon-search.min.js` | https://github.com/xtermjs/xterm.js | MIT |
| highlight.js | 11.11.1 | `highlightjs/highlight.min.js`, `highlightjs/languages/shell.min.js`, `highlightjs/languages/yaml.min.js` | https://github.com/highlightjs/highlight.js | BSD-3-Clause |
| Nord highlight.js theme | 0.1.0 | `highlightjs/styles/nord.min.css` | https://github.com/arcticicestudio/nord-highlightjs, bundled in highlight.js 11.11.1 | MIT |

No upstream `NOTICE` file exists for any of these components.

## xterm.js

```text
Copyright (c) 2017-2019, The xterm.js authors (https://github.com/xtermjs/xterm.js)
Copyright (c) 2014-2016, SourceLair Private Company (https://www.sourcelair.com)
Copyright (c) 2012-2013, Christopher Jeffrey (https://github.com/chjj/)
```

### xterm.js fit addon

```text
Copyright (c) 2019, The xterm.js authors (https://github.com/xtermjs/xterm.js)
```

### xterm.js search addon

```text
Copyright (c) 2017, The xterm.js authors (https://github.com/xtermjs/xterm.js)
```

## Nord highlight.js theme

`nord.min.css` has no version banner. It is identical to the file in the
highlight.js 11.11.1 CDN release ([highlightjs/cdn-release](https://github.com/highlightjs/cdn-release)),
and the upstream `nord.css` records version 0.1.0.

```text
Copyright (c) 2017-present Arctic Ice Studio <development@arcticicestudio.com>
Copyright (c) 2017-present Sven Greb <development@svengreb.de>
Copyright (c) 2016-present Sven Greb <development@svengreb.de> (https://www.svengreb.de)
```

## MIT License

Applies to xterm.js, its two addons and the Nord theme, each with the copyright
notice listed above.

```text
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## highlight.js

The `highlight.min.js` banner reads "(c) 2006-2024 Josh Goebel and other
contributors". The `shell` and `yaml` grammars are part of highlight.js 11.11.1
and credit their authors: Stefan Wienert and Carl Baxter (`yaml`), and TSUYUSATO
Kitsune (`shell`).

```text
BSD 3-Clause License

Copyright (c) 2006, Ivan Sagalaev.
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

* Redistributions of source code must retain the above copyright notice, this
  list of conditions and the following disclaimer.

* Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.

* Neither the name of the copyright holder nor the names of its
  contributors may be used to endorse or promote products derived from
  this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
