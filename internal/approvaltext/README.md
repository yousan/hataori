# Approval text and Unicode

`Validate` runs before a request is stored or its event is emitted. It accepts
at most 32 KiB of UTF-8 and reports the first refused character's one-based
UTF-8 byte position. Line feeds and tabs are allowed. Other controls and format characters, Unicode's
Default_Ignorable_Code_Point characters and blank letters are refused.

The only exceptions are U+200D and U+FE0F inside emoji sequences listed by
Unicode 18.0. The whitelist includes the qualification forms in `emoji-test.txt`
and emoji-style variation sequences. Tag characters remain refused, including
in listed flag sequences. Longest listed sequences are matched first; an extra
selector or joiner is then refused at its original position.

Sources are the Unicode Consortium's [emoji test data](https://www.unicode.org/Public/18.0.0/emoji/emoji-test.txt),
[emoji variation sequences](https://www.unicode.org/Public/18.0.0/ucd/emoji/emoji-variation-sequences.txt)
and [derived core properties](https://www.unicode.org/Public/18.0.0/ucd/DerivedCoreProperties.txt),
under the accompanying `LICENSE.txt`. `generate.py` pins their SHA-256 checksums;
run `go generate` to reproduce the tables. The unmodified emoji test data is
retained under `testdata/18.0/`. To use captured downloads without network
access, run `python3 generate.py --data-dir DIRECTORY`.
