import {
  findIncludedsFromRelationships,
  findIncludedFromRelationship,
  findIncludedVariables,
  getTemplateDetails,
  getGithubUrlFromTemplateDetails,
} from 'src/templates/utils/'
import {TemplateType} from 'src/types'

const includeds = [
  {type: TemplateType.Cell, id: '1', attributes: {id: 'a'}},
  {type: TemplateType.View, id: '3'},
  {type: TemplateType.Variable, id: '3'},
  {type: TemplateType.Variable, id: '1'},
]
const relationships = [{type: TemplateType.Cell, id: '1'}]

describe('Templates utils', () => {
  describe('findIncludedsFromRelationships', () => {
    it('finds item in included that matches relationship', () => {
      const actual = findIncludedsFromRelationships(includeds, relationships)
      const expected = [
        {type: TemplateType.Cell, id: '1', attributes: {id: 'a'}},
      ]

      expect(actual).toEqual(expected)
    })
  })

  describe('findIncludedFromRelationship', () => {
    it('finds included that matches relationship', () => {
      const actual = findIncludedFromRelationship(includeds, relationships[0])
      const expected = {type: TemplateType.Cell, id: '1', attributes: {id: 'a'}}

      expect(actual).toEqual(expected)
    })
  })

  describe('findIncludedVariables', () => {
    it('finds included that matches relationship', () => {
      const actual = findIncludedVariables(includeds)
      const expected = [
        {type: TemplateType.Variable, id: '3'},
        {type: TemplateType.Variable, id: '1'},
      ]

      expect(actual).toEqual(expected)
    })
  })

  describe('getTemplateDetailsGithub', () => {
    it('Confirm get template details returns the proper format for github url', () => {
      const actual = getTemplateDetails(
        'https://github.com/influxdata/community-templates/blob/master/modbus/modbus.yml'
      )
      const expected = {
        directory: 'modbus',
        templateExtension: 'yml',
        templateName: 'modbus',
      }

      expect(actual).toEqual(expected)
    })
  })

  describe('getTemplateDetailsSource', () => {
    it('Confirm get template details returns the proper format for Source url', () => {
      const actual = getTemplateDetails('file://')
      const expected = {
        directory: '',
        templateExtension: '',
        templateName: '',
      }

      expect(actual).toEqual(expected)
    })
  })

  describe('getTemplateDetailsError', () => {
    it('Confirm get template details returns the proper format for wrong url', () => {
      expect(() => {
        getTemplateDetails('octopus')
      }).toThrowError()
    })
  })

  describe('getTemplateDetailsError', () => {
    it('Confirm get template details returns the proper format for wrong url', () => {
      expect(() => {
        getTemplateDetails('octopus')
      }).toThrowError()
    })
  })

  describe('getGithubUrlFromTemplateDetailsTest1', () => {
    it('Get back the proper url', () => {
      const actual = getGithubUrlFromTemplateDetails('modbus', 'modbus', 'yml')
      const expected =
        'https://github.com/influxdata/community-templates/blob/master/modbus/modbus.yml'

      expect(actual).toEqual(expected)
    })
  })

  describe('getGithubUrlFromTemplateDetailsTest2', () => {
    it('Get back the proper url', () => {
      const actual = getGithubUrlFromTemplateDetails('docker', 'docker', 'yml')
      const expected =
        'https://github.com/influxdata/community-templates/blob/master/docker/docker.yml'

      expect(actual).toEqual(expected)
    })
  })

  describe('getGithubUrlFromTemplateDetailsTest3', () => {
    it('Get back the proper url', () => {
      const actual = getGithubUrlFromTemplateDetails(
        'kafka',
        'kafka-template',
        'yml'
      )
      const expected =
        'https://github.com/influxdata/community-templates/blob/master/kafka/kafka-template.yml'

      expect(actual).toEqual(expected)
    })
  })
})
