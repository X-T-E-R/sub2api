"""Core-backup-only identity contract; image and revision are inseparable."""
OLD_PAIR = ('sha256:47cb579cfbccec8fcb262738a330ab343f80c082fbb17aff88a7cdd3d376fced',
            '2dc8640c1d9b23e8c9cf24b332baaba463bde2ca')
NEW_PAIR = ('sha256:a04689c655a9293b2805ab65e94c1c6439179c03fe4c5faa90b7a3e5d7094b73',
            '16cd6eab05b4ef605d08d7980c2d2853741d5a17')
PREHEADER_PAIR = ('sha256:06ee81a40b6a67759d9d286a302b1ac25adf236f8bcbe623b2e33d34c20384f4',
                  'f18388a440ddce04c5da0d0917ba2e4db04a5b0e')
ARGON502_PAIR = ('sha256:a09c7ecad5c7c722dddd3c925c0f8510a2cbafe57666f2a1f9875c2b47439f58',
                  '7c12de7380be0142e18bcdc6397b6537f2af9968')
NAILONG_CONFIG_PAIR = ('sha256:f5b10dd50d6c358570b3f4d98e25351f525f5e280add7567ddb5069f4b1ab8e5',
                       'f18388a440ddce04c5da0d0917ba2e4db04a5b0e')
# Docker 20.10 config id of the fc8997384 image loaded on Nailong. Not the Docker 29 manifest list id.
FC899_NAILONG_PAIR = ('sha256:8a087d23f70de512fcb9b064b9b088fa453650dbf2e8d4b05929f9130c0937a7',
                      'fc899738487a38a7d40664fdf92bc90375fbaf7b')
# Docker 20.10 config id of the 1d14bbdc3 image loaded on Nailong. Not the Docker 29 manifest list id.
REWRITE1D14_NAILONG_PAIR = ('sha256:ad403de503301cc681c18b961a3532a8c739b4bed1b1b7de3043a9073b3c5e61',
                            '1d14bbdc38c25e4279f6948c7128c4a43e6ef985')
SEARCH9DAC_NAILONG_PAIR = ('sha256:bd872ec2569fe2a1be2b72beffd8c0c28e194dd53c1b29a922ae22c2bb513b15',
                          '9dac8d13117c42a14e61bcc7af65c26d8ef987fd')
ALLOWED_PAIRS = frozenset((OLD_PAIR, NEW_PAIR, PREHEADER_PAIR, ARGON502_PAIR))
ADMITTED_PAIRS = ALLOWED_PAIRS | frozenset((NAILONG_CONFIG_PAIR, FC899_NAILONG_PAIR, REWRITE1D14_NAILONG_PAIR, SEARCH9DAC_NAILONG_PAIR))


def admits(image, revision):
    return (image, revision) in ADMITTED_PAIRS


def runtime_matches(manifest, runtime):
    return (admits(manifest.get('source_image'), manifest.get('revision'))
            and (runtime.get('image'), runtime.get('revision')) ==
                (manifest.get('source_image'), manifest.get('revision'))
            and runtime.get('source') == manifest.get('source'))
