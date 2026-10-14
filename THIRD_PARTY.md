# Third-party material

ermite-runner's executor (`internal/executor`) is a Go port of parts of
[actions/runner](https://github.com/actions/runner) (C#), at commit `5f0b3dd`.
Texts of messages, limits and the behaviour of the workflow commands, problem
matchers, masker and step timeouts are taken from its source, and some unit
tests are table versions of its L0 tests:

| Test here | Source test in `src/Test/L0` |
| --- | --- |
| `TestValidateLinesAndColumns` | `Worker/ActionCommandManagerL0.cs` `IssueCommandInvalidColumns` |
| `TestMatcherConfigValidate` | `Worker/IssueMatcherL0.cs` `Config_Validate_*` |
| `TestMatcherLoopAccumulatesState`, `TestMatcherNonLoop`, `TestMatcherSinglePattern` | `Worker/IssueMatcherL0.cs` `Matcher_*` |
| `TestDefaultSecretMaskers`, `TestSecretSectionMasking` | `HostContextL0.cs` `DefaultSecretMaskers`, `SecretSectionMasking` |
| `TestAddMaskCommand` | `Worker/ActionCommandManagerL0.cs` `AddMaskWithMultilineValue` |
| `TestCatastrophicMatcherIsRemovedAfterThreeTimeouts` | `Worker/OutputManagerL0.cs` `MatcherTimeout` |

actions/runner is licensed under the MIT License:

    The MIT License (MIT)
    Copyright (c) 2019 GitHub

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

github.com/dlclark/regexp2 (MIT) is a dependency: a port of the .NET regular expression engine, used for problem matchers.
