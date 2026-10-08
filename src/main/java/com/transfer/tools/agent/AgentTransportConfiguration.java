package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.BeansException;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.beans.factory.config.BeanPostProcessor;
import org.springframework.core.env.Environment;
import org.springframework.stereotype.Component;
import org.springframework.web.client.RestTemplate;
import java.lang.reflect.Field;
import java.util.Collections;
import java.util.IdentityHashMap;
import java.util.Set;

/** Includes constructor-created private RestTemplates used by the existing executors. */
@Component
public class AgentTransportConfiguration implements BeanPostProcessor {
    private final ObjectProvider<AgentRegistryService> registry;
    private final ObjectProvider<AgentBroker> broker;
    private final ObjectProvider<AgentIdentityStore> identity;
    private final ObjectProvider<DistributedConfig> config;
    private final Environment environment;
    private final Set<RestTemplate> wrapped = Collections.newSetFromMap(new IdentityHashMap<>());

    public AgentTransportConfiguration(ObjectProvider<AgentRegistryService> registry, ObjectProvider<AgentBroker> broker,
                                       ObjectProvider<AgentIdentityStore> identity, ObjectProvider<DistributedConfig> config,
                                       Environment environment) {
        this.registry = registry; this.broker = broker; this.identity = identity;
        this.config = config; this.environment = environment;
    }

    @Override public Object postProcessBeforeInitialization(Object bean, String name) throws BeansException {
        Class<?> type = bean.getClass();
        if (!type.getName().startsWith("com.transfer.tools.")) return bean;
        while (type != null && type.getName().startsWith("com.transfer.tools.")) {
            for (Field field : type.getDeclaredFields()) {
                if (!RestTemplate.class.isAssignableFrom(field.getType())) continue;
                try {
                    field.setAccessible(true);
                    RestTemplate template = (RestTemplate)field.get(bean);
                    if (template != null && wrapped.add(template))
                        template.setRequestFactory(new AgentHttpRequestFactory(template.getRequestFactory(),
                                registry, broker, identity, config, environment));
                } catch (IllegalAccessException e) { throw new IllegalStateException("Cannot configure Agent HTTP transport", e); }
            }
            type = type.getSuperclass();
        }
        return bean;
    }
}
