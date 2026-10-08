package com.transfer.tools.model;

import lombok.Data;

/** Literal OSS object-key mapping. A null mapping preserves the source key. */
@Data
public class OssKeyMapping {
    public enum Mode { PRESERVE, ADD_PREFIX, REPLACE_PREFIX, EXACT_KEY }
    private Mode mode;
    private String sourcePrefix;
    private String targetPrefix;
    private String targetKey;
}
