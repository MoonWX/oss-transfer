package com.transfer.tools.util;

import com.transfer.tools.model.OssKeyMapping;
import com.transfer.tools.service.TransferValidationException;
import java.nio.CharBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.StandardCharsets;
import java.util.Objects;

/** Object keys are literal strings: never trim whitespace or normalize slashes. */
public final class OssKeyMapper {
    private OssKeyMapper() { }

    public static String destinationKey(OssKeyMapping mapping, String sourceKey) {
        requireKey(sourceKey, "sourceKey");
        validateShape(mapping);
        String result = sourceKey;
        if (mapping != null) switch (mapping.getMode()) {
            case ADD_PREFIX:
                String prefix = mapping.getTargetPrefix();
                result = prefix + (prefix.endsWith("/") ? "" : "/") + sourceKey;
                break;
            case REPLACE_PREFIX:
                if (!sourceKey.startsWith(mapping.getSourcePrefix()))
                    throw invalid("sourceKey does not match sourcePrefix");
                result = nullToEmpty(mapping.getTargetPrefix()) + sourceKey.substring(mapping.getSourcePrefix().length());
                break;
            case EXACT_KEY: result = mapping.getTargetKey(); break;
            case PRESERVE: break;
            default: throw invalid("Unsupported mapping mode");
        }
        requireKey(result, "destinationKey");
        return result;
    }

    public static void validate(OssKeyMapping mapping, String sourcePrefix, String objectKey) {
        if (present(sourcePrefix) && present(objectKey))
            throw new TransferValidationException("INVALID_SOURCE_SELECTION", "objectKey and prefix are mutually exclusive");
        if (present(objectKey)) requireKey(objectKey, "objectKey");
        if (present(sourcePrefix)) requireKey(sourcePrefix, "prefix");
        validateShape(mapping);
        if (mapping == null) return;
        if (mapping.getMode() == OssKeyMapping.Mode.EXACT_KEY && !present(objectKey))
            throw invalid("EXACT_KEY requires one source objectKey");
        if (mapping.getMode() == OssKeyMapping.Mode.REPLACE_PREFIX
                && (!present(sourcePrefix) || !sourcePrefix.equals(mapping.getSourcePrefix())))
            throw invalid("REPLACE_PREFIX requires the same nonempty source filter prefix");
        if (present(objectKey)) destinationKey(mapping, objectKey);
    }

    /** Null and explicit PRESERVE describe the same immutable mapping. */
    public static boolean equivalent(OssKeyMapping first, OssKeyMapping second) {
        if (isPreserve(first) && isPreserve(second)) return true;
        return Objects.equals(first, second);
    }

    private static boolean isPreserve(OssKeyMapping value) {
        return value == null || value.getMode() == OssKeyMapping.Mode.PRESERVE;
    }

    private static void validateShape(OssKeyMapping mapping) {
        if (mapping == null) return;
        if (mapping.getMode() == null) throw invalid("keyMapping.mode is required");
        switch (mapping.getMode()) {
            case PRESERVE:
                if (present(mapping.getSourcePrefix()) || present(mapping.getTargetPrefix()) || present(mapping.getTargetKey()))
                    throw invalid("PRESERVE does not accept prefix or targetKey fields");
                break;
            case ADD_PREFIX:
                requireKey(mapping.getTargetPrefix(), "targetPrefix");
                if (present(mapping.getSourcePrefix()) || present(mapping.getTargetKey()))
                    throw invalid("ADD_PREFIX only accepts targetPrefix");
                break;
            case REPLACE_PREFIX:
                requireKey(mapping.getSourcePrefix(), "sourcePrefix");
                if (present(mapping.getTargetPrefix())) requireKey(mapping.getTargetPrefix(), "targetPrefix");
                if (present(mapping.getTargetKey())) throw invalid("REPLACE_PREFIX does not accept targetKey");
                break;
            case EXACT_KEY:
                requireKey(mapping.getTargetKey(), "targetKey");
                if (present(mapping.getSourcePrefix()) || present(mapping.getTargetPrefix()))
                    throw invalid("EXACT_KEY only accepts targetKey");
                break;
            default: throw invalid("Unsupported mapping mode");
        }
    }

    private static void requireKey(String value, String name) {
        if (!present(value)) throw invalid(name + " must not be empty");
        try {
            if (StandardCharsets.UTF_8.newEncoder().encode(CharBuffer.wrap(value)).remaining() > 1024)
                throw invalid(name + " exceeds 1024 UTF-8 bytes");
        } catch (CharacterCodingException malformed) {
            throw invalid(name + " contains invalid Unicode");
        }
    }

    private static boolean present(String value) { return value != null && !value.isEmpty(); }
    private static String nullToEmpty(String value) { return value == null ? "" : value; }
    private static TransferValidationException invalid(String message) {
        return new TransferValidationException("INVALID_KEY_MAPPING", message);
    }
}
