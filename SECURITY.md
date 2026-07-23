# Security policy

## Supported versions

This project has not published a tagged release. Until the first release,
security fixes are made on `main`; consumers should upgrade to the fixing
revision after reviewing it for their deployment. After releases begin, the
latest tagged release will be supported. Older releases may be asked to
upgrade when a fix depends on parser, compiler, or toolchain changes that
cannot be backported safely.

## Reporting a vulnerability

Use [GitHub's private vulnerability reporting
form](https://github.com/thesyncim/slopjson/security/advisories/new). Do not put
exploit details, private inputs, or a reproducer in a public issue. If private
reporting is unavailable, open a public issue containing only a request for a
private contact channel.

Include the affected revision, Go revision, architecture, build flags, smallest
available reproducer, and expected impact. Reports about out-of-bounds access,
stale or hidden pointers, data retained past its documented lifetime, parser
acceptance differences, denial of service, or unsafe custom-method dispatch are
in scope.

The report will be kept private while it is reproduced and a fix is prepared.
Please allow time for the fix to pass the generic, SIMD, race, checkptr,
differential, and performance gates before disclosure.
