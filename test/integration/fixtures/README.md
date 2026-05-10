# Integration test fixtures

These files are produced by `scripts/gen-fixtures.py`. Each carries
"owui-cee-proxy routing fixture" somewhere visible so failures and
extracted snippets are unambiguous.

| File           | MIME                                                                                | Routes to (default config)            |
|----------------|--------------------------------------------------------------------------------------|----------------------------------------|
| sample.txt     | text/plain                                                                           | docling                                |
| sample.html    | text/html                                                                            | docling                                |
| sample.md      | text/markdown                                                                        | docling                                |
| sample.csv     | text/csv                                                                             | docling                                |
| sample.json    | application/json                                                                     | docling                                |
| sample.xml     | application/xml                                                                      | docling                                |
| sample.pdf     | application/pdf                                                                      | docling                                |
| sample.png     | image/png                                                                            | docling                                |
| sample.jpeg    | image/jpeg                                                                           | docling                                |
| sample.docx    | application/vnd.openxmlformats-officedocument.wordprocessingml.document              | docling                                |
| sample.xlsx    | application/vnd.openxmlformats-officedocument.spreadsheetml.sheet                    | docling                                |
| sample.pptx    | application/vnd.openxmlformats-officedocument.presentationml.presentation            | docling                                |
| sample.rtf     | application/rtf                                                                      | kreuzberg (fallback)                   |
| sample.eml     | message/rfc822                                                                       | kreuzberg                              |
| sample.odt     | application/vnd.oasis.opendocument.text                                              | kreuzberg                              |
| sample.epub    | application/epub+zip                                                                 | kreuzberg                              |
| sample.msg     | application/vnd.ms-outlook                                                           | kreuzberg                              |

`sample.msg` is a hand-built minimal CFB — enough magic bytes plus
three MAPI string properties (subject / body / sender). It is NOT a
real Outlook export and may not round-trip in Outlook itself, but it
satisfies Kreuzberg's `.msg` parser end-to-end.

To regenerate any of these, run `python3 scripts/gen-fixtures.py`
from the repository root.
